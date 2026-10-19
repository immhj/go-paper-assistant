package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

// Keep QQ search results below the 12,000-rune stream guard used by deep reading.
// When all candidates do not fit, whole low-priority candidates are omitted from the
// QQ message; a candidate that is shown is never truncated. The Web is not limited.
const candidateResponseRunes = 10_500

var paperCountPattern = regexp.MustCompile(`(\d+)\s*篇`)

const maxSearchPapers = 10

func (a *Application) search(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, query string, plan searchToolPlan, deliver deliverer) error {
	if query == "" {
		return deliver.Text(ctx, conversation, helpText(), nil, 0)
	}
	unlock := a.lockConversation(conversation.ID)
	defer unlock()

	// The Web indicator already reads "正在执行：论文检索"; only QQ gets a message.
	if err := announce(ctx, deliver, conversation, "", "", conversationHeader(conversation)+"\n正在执行论文检索……"); err != nil {
		return err
	}
	current := parseSearchContext(conversation.CurrentContext, conversation.Title)
	messages, _ := a.store.ListRecentUserMessages(ctx, conversation.ID, 10)

	spec := current.Spec
	specCost := float64(0)
	// The data source is decided in Go from the user's own message, never from the
	// model-written tool query (ADR 0018).
	sourceValue := specSourceValue(searchSource(plan.Mode, input.Content, current.Spec))
	switch plan.Mode {
	case "continue":
		if len(spec.Concepts) == 0 {
			plan.Mode = "restart"
		} else if spec.Source != sourceValue {
			spec.Source = sourceValue
			current.SpecVersion++
		}
	case "refine":
		previous := spec
		spec, specCost = a.compileSpec(ctx, query, messages, current.Spec)
		spec.Source = sourceValue
		if !sameSearchSpec(previous, spec) {
			current.SpecVersion++
		}
	case "restart":
		spec, specCost = a.compileSpec(ctx, query, messages, domain.SearchSpec{})
		spec.Source = sourceValue
		current.Name = query
		current.SpecVersion++
	}
	if plan.Mode == "restart" && len(spec.Concepts) == 0 {
		spec, specCost = a.compileSpec(ctx, query, messages, domain.SearchSpec{})
		spec.Source = sourceValue
		current.Name = query
		current.SpecVersion++
	}
	current.Spec = spec
	a.recordToolUsage(input.MessageID, domain.ModelUsage{CostUSD: specCost})

	var excluded []domain.PaperIdentity
	var err error
	if plan.ExcludeSeen {
		excluded, err = a.seenPaperIdentities(ctx, current)
		if err != nil {
			slog.Error("load complete seen-paper exclusion set", "conversation_id", conversation.ID, "error", err)
			return errors.Join(err, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n无法完整读取已展示论文列表，本次没有执行继续检索，请稍后重试。", nil, 0))
		}
	}
	candidates, err := a.papers.Search(ctx, domain.PaperSearchRequest{Spec: spec, Limit: plan.Count, Exclude: excluded})
	if errors.Is(err, domain.ErrSourceQuotaExhausted) {
		// A spent OpenAlex daily budget is an expected condition, not a failed task. The user
		// notice stays provider-neutral: upstream services and budgets are internal details.
		slog.Warn("paper source daily quota exhausted", "conversation_id", conversation.ID, "source", spec.EffectiveSource())
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\nIEEE 检索今日次数已达上限，请稍后再试，或改用 arXiv 检索。", nil, specCost)
	}
	if err != nil {
		slog.Error("search_papers tool failed", "conversation_id", conversation.ID, "message_id", input.MessageID, "count", plan.Count, "mode", plan.Mode, "exclude_seen", plan.ExcludeSeen, "error", err)
		return errors.Join(err, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n论文检索失败，请稍后重试。", nil, 0))
	}
	if len(candidates) == 0 {
		message := "没有找到同时满足这些概念的论文，请调整条件后重试。"
		if plan.ExcludeSeen {
			message = "没有找到更多未展示的候选论文，可以放宽条件或换一个研究方向。"
		}
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+message, []domain.ShownPaper{}, specCost)
	}
	for index := range candidates {
		candidates[index].Abstract = normalizedAbstract(candidates[index].Abstract)
	}
	judgments, judgmentCost := a.judgeRelevance(ctx, spec, candidates)
	a.recordToolUsage(input.MessageID, domain.ModelUsage{CostUSD: judgmentCost})

	shown := make([]domain.ShownPaper, 0, len(candidates))
	for index := range candidates {
		stored, storeErr := a.store.UpsertPaper(ctx, candidates[index].Paper)
		if storeErr != nil {
			return storeErr
		}
		candidates[index].Paper = stored
		if stored.CitationCount > 0 {
			candidates[index].CitationKnown = true
		}
		ordinal := current.Ordinals[fmt.Sprintf("%d", stored.ID)]
		tier, label, note := quality(candidates[index])
		shownItem := domain.ShownPaper{
			PaperID: stored.ID, Ordinal: ordinal, QualityTier: tier, QualityLabel: label, QualityNote: note,
			OpenAccessPDFURL: candidates[index].OpenAccessPDFURL, ArxivID: candidates[index].ArxivID,
			Source: stored.Source, LandingURL: stored.LandingURL(),
			Title: stored.Title, AbstractMissing: stored.Abstract == nil,
		}
		if stored.TLDR != nil {
			shownItem.TLDR = strings.TrimSpace(*stored.TLDR)
		}
		if stored.DOI != nil {
			shownItem.DOI = *stored.DOI
		}
		if judgment, ok := judgments[index]; ok {
			level := judgment.Level
			shownItem.Relevance = &level
			shownItem.RelevanceReason = judgment.Reason
			shownItem.SummaryZH = judgment.SummaryZH
		}
		shown = append(shown, shownItem)
	}
	sort.SliceStable(shown, func(i, j int) bool {
		return relevanceScore(shown[i].Relevance) > relevanceScore(shown[j].Relevance)
	})

	nextOrdinal := current.NextOrdinal
	for index := range shown {
		if shown[index].Ordinal == 0 {
			shown[index].Ordinal = nextOrdinal
			nextOrdinal++
		}
	}
	// QQ is limited by its single-message budget; the Web shows every candidate as a card.
	// A conversation lives on exactly one channel, so each rendering records only the
	// ordinals it actually delivered.
	visible, response := fitCandidateResponse(conversation, candidates, shown, plan.Count, candidateResponseRunes)
	reply := searchReply{
		QQ:  searchChannelReply{Text: response, Shown: visible, Context: contextWithOrdinals(current, visible)},
		Web: searchChannelReply{Text: renderSearchSummary(len(shown), plan.Count), Shown: shown, Context: contextWithOrdinals(current, shown)},
	}
	if err := deliver.SearchResult(ctx, conversation, reply, specCost+judgmentCost); err != nil {
		return err
	}
	markSearchDelivered(ctx, deliver, reply)
	return nil
}

// contextWithOrdinals returns the encoded conversation context after recording the ordinals of
// the delivered papers. It copies the ordinal map so the two channel renderings stay independent.
func contextWithOrdinals(current domain.SearchContext, delivered []domain.ShownPaper) json.RawMessage {
	ordinals := make(map[string]int, len(current.Ordinals)+len(delivered))
	for key, value := range current.Ordinals {
		ordinals[key] = value
	}
	current.Ordinals = ordinals
	for _, item := range delivered {
		key := fmt.Sprintf("%d", item.PaperID)
		if _, exists := current.Ordinals[key]; !exists {
			current.Ordinals[key] = item.Ordinal
			current.NextOrdinal++
		}
	}
	encoded, _ := json.Marshal(current)
	return encoded
}

type searchToolPlan struct {
	Count       int    `json:"count"`
	Mode        string `json:"mode"`
	ExcludeSeen bool   `json:"exclude_seen"`
}

func (a *Application) planPaperSearch(ctx context.Context, query string, history []domain.Message, current domain.SearchContext) (searchToolPlan, float64) {
	fallback := fallbackSearchPlan(query, current)
	if !a.model.Available() {
		return fallback, 0
	}
	var historyText strings.Builder
	for _, message := range history {
		if message.Content != nil {
			historyText.WriteString("- ")
			historyText.WriteString(*message.Content)
			historyText.WriteByte('\n')
		}
	}
	currentSpec, _ := json.Marshal(current.Spec)
	tool := deepseek.ToolDefinition{Type: "function", Function: deepseek.ToolFunction{
		Name:        "search_papers",
		Description: "检索论文。count 是本次希望返回的新论文数；mode=continue 表示规格不变继续找，refine 表示修改当前规格，restart 表示新方向；exclude_seen 表示排除本会话已展示论文。",
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"count":        map[string]any{"type": "integer", "minimum": 1, "maximum": maxSearchPapers},
				"mode":         map[string]any{"type": "string", "enum": []string{"continue", "refine", "restart"}},
				"exclude_seen": map[string]any{"type": "boolean"},
			},
			"required": []string{"count", "mode", "exclude_seen"},
		},
	}}
	call, usage, err := a.model.CallTool(ctx, deepseek.ToolRequest{
		ToolName: "search_papers", Tools: []deepseek.ToolDefinition{tool}, Messages: []deepseek.Message{
			{Role: "system", Content: `你是论文检索 Agent。每次必须且只能调用一次 search_papers。理解用户是在继续同一方向、修改当前条件还是开始新方向，并准确提取数量。规则："再来/再找/继续/更多N篇"=>continue 且 exclude_seen=true；在当前方向增加年份或排除条件=>refine 且 exclude_seen=true；明确新主题=>restart。count 未指定时为5，最大10。不要把数量当作学术关键词。`},
			{Role: "user", Content: "当前方向：" + current.Name + "\n当前规格：" + string(currentSpec) + "\n已展示论文数：" + strconv.Itoa(len(current.Ordinals)) + "\n本轮用户消息：" + query + "\n最近用户消息：\n" + historyText.String()},
		},
	})
	if err != nil {
		slog.Error("search agent tool decision failed; using deterministic fallback", "error", err)
		return fallback, usage.CostUSD
	}
	if call.Name != "search_papers" || len(call.Arguments) > 2048 {
		slog.Error("search agent returned an invalid tool call", "name", call.Name, "arguments_bytes", len(call.Arguments))
		return fallback, usage.CostUSD
	}
	decoder := json.NewDecoder(strings.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	var plan searchToolPlan
	if err = decoder.Decode(&plan); err != nil {
		slog.Error("decode search_papers arguments; using deterministic fallback", "error", err)
		return fallback, usage.CostUSD
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		slog.Error("search_papers arguments contain trailing JSON; using deterministic fallback")
		return fallback, usage.CostUSD
	}
	if plan.Count < 1 || plan.Count > maxSearchPapers || (plan.Mode != "continue" && plan.Mode != "refine" && plan.Mode != "restart") {
		slog.Error("search_papers arguments failed validation", "count", plan.Count, "mode", plan.Mode)
		return fallback, usage.CostUSD
	}
	// Explicit numeric and continuation phrases are deterministic constraints,
	// not suggestions. The Agent chooses ambiguous intent; Go enforces what the
	// user stated literally so “再检索5篇” cannot become a restart.
	if count, explicit := explicitRequestedPaperCount(query); explicit {
		plan.Count = count
	}
	if explicitContinue(query, current) {
		plan.Mode = "continue"
		plan.ExcludeSeen = true
	}
	return plan, usage.CostUSD
}

func fallbackSearchPlan(query string, current domain.SearchContext) searchToolPlan {
	plan := searchToolPlan{Count: requestedPaperCount(query), Mode: "restart"}
	if mode, explicit := fallbackCurrentMode(query, current); explicit {
		plan.Mode = mode
		plan.ExcludeSeen = true
	}
	return plan
}

func explicitContinue(query string, current domain.SearchContext) bool {
	if len(current.Spec.Concepts) == 0 {
		return false
	}
	lower := strings.ToLower(query)
	return strings.Contains(lower, "再来") || strings.Contains(lower, "再找") || strings.Contains(lower, "继续") || strings.Contains(lower, "更多") || strings.Contains(lower, "另外") || strings.Contains(lower, "再检索")
}

func fallbackCurrentMode(query string, current domain.SearchContext) (string, bool) {
	if len(current.Spec.Concepts) == 0 {
		return "", false
	}
	lower := strings.ToLower(query)
	refines := strings.Contains(lower, "限定") || strings.Contains(lower, "以后") || strings.Contains(lower, "之前") || strings.Contains(lower, "排除") || strings.Contains(lower, "不要") || strings.Contains(lower, "只要") || strings.Contains(lower, "年份")
	if refines {
		return "refine", true
	}
	if explicitContinue(query, current) {
		return "continue", true
	}
	return "", false
}

func requestedPaperCount(query string) int {
	if count, explicit := explicitRequestedPaperCount(query); explicit {
		return count
	}
	return 5
}

func explicitRequestedPaperCount(query string) (int, bool) {
	if match := paperCountPattern.FindStringSubmatch(query); len(match) == 2 {
		count, err := strconv.Atoi(match[1])
		if err == nil && count > 0 {
			if count > maxSearchPapers {
				return maxSearchPapers, true
			}
			return count, true
		}
	}
	for count, numeral := range []string{"", "一", "两", "三", "四", "五"} {
		if count > 0 && strings.Contains(query, numeral+"篇") {
			return count, true
		}
	}
	return 0, false
}

func (a *Application) seenPaperIdentities(ctx context.Context, current domain.SearchContext) ([]domain.PaperIdentity, error) {
	identities := make([]domain.PaperIdentity, 0, len(current.Ordinals))
	for paperID := range current.Ordinals {
		id, err := strconv.ParseUint(paperID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse seen paper id %q: %w", paperID, err)
		}
		paper, err := a.store.GetPaper(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load seen paper %d: %w", id, err)
		}
		if paper.Source == "" || paper.ExternalID == "" {
			return nil, fmt.Errorf("seen paper %d has no stable identity", id)
		}
		identities = append(identities, domain.PaperIdentity{Source: paper.Source, ExternalID: paper.ExternalID})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Source == identities[j].Source {
			return identities[i].ExternalID < identities[j].ExternalID
		}
		return identities[i].Source < identities[j].Source
	})
	return identities, nil
}

func (a *Application) saveSearchResultWithRetry(ctx context.Context, userID, conversationID uint64, current json.RawMessage, message domain.Message) error {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if last = a.store.SaveSearchResult(ctx, userID, conversationID, current, message); last == nil {
			return nil
		}
		if errors.Is(last, dao.ErrNotFound) || errors.Is(last, dao.ErrDuplicate) {
			return last
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(last, ctx.Err())
		case <-timer.C:
		}
	}
	return last
}

func (a *Application) compileSpec(ctx context.Context, query string, history []domain.Message, existing domain.SearchSpec) (domain.SearchSpec, float64) {
	fallback := existing
	if len(fallback.Concepts) == 0 {
		fallback = domain.SearchSpec{Concepts: []domain.SearchConcept{{Concept: query, Synonyms: []string{query}}}}
	}
	if !a.model.Available() {
		return fallback, 0
	}
	var historyText strings.Builder
	for _, message := range history {
		if message.Content != nil {
			historyText.WriteString("- ")
			historyText.WriteString(*message.Content)
			historyText.WriteByte('\n')
		}
	}
	existingJSON, _ := json.Marshal(existing)
	content, usage, err := a.model.Complete(ctx, deepseek.Request{JSON: true, Messages: []deepseek.Message{
		{Role: "system", Content: `你把中文论文检索需求编译成结构化检索规格。只输出 JSON：{"concepts":[{"concept":"中文概念","synonyms":["English term"]}],"yearFrom":2020,"yearTo":null,"exclude":["survey"]}。同义词用适合学术搜索的英文；不要编造论文。`},
		{Role: "user", Content: "当前需求：" + query + "\n当前规格：" + string(existingJSON) + "\n本会话最近用户原话：\n" + historyText.String()},
	}})
	if err != nil {
		// Degrading to the raw query silently makes retrieval quality drop with no
		// visible symptom, so the fallback must always leave a trace.
		slog.Error("compile search spec failed; falling back to the raw query", "error", err)
		return fallback, 0
	}
	var spec domain.SearchSpec
	if unmarshalErr := json.Unmarshal([]byte(stripCodeFence(content)), &spec); unmarshalErr != nil || len(spec.Concepts) == 0 {
		slog.Error("compile search spec returned unusable JSON; falling back to the raw query",
			"error", unmarshalErr, "concepts", len(spec.Concepts))
		return fallback, usage.CostUSD
	}
	return spec, usage.CostUSD
}

func (a *Application) judgeRelevance(ctx context.Context, spec domain.SearchSpec, candidates []domain.Candidate) (map[int]domain.RelevanceJudgment, float64) {
	result := make(map[int]domain.RelevanceJudgment)
	if !a.model.Available() || len(candidates) == 0 {
		return result, 0
	}
	type item struct {
		Index    int     `json:"index"`
		Title    string  `json:"title"`
		Abstract *string `json:"abstract"`
	}
	items := make([]item, 0, len(candidates))
	for index, candidate := range candidates {
		// Papers without an abstract are never sent to the model: their judgments would be
		// discarded anyway, and the model must not guess from a title alone.
		if candidate.Abstract == nil {
			continue
		}
		items = append(items, item{Index: index, Title: candidate.Title, Abstract: candidate.Abstract})
	}
	if len(items) == 0 {
		return result, 0
	}
	input, _ := json.Marshal(map[string]any{"spec": spec, "papers": items})
	content, usage, err := a.model.Complete(ctx, deepseek.Request{JSON: true, Messages: []deepseek.Message{
		{Role: "system", Content: `只依据每篇论文给出的 title 和 abstract 完成两件事：判断其与检索规格的相关性，并用中文概括 abstract。只输出 JSON：{"items":[{"index":0,"level":"high|medium|low","reason":"不超过60字","summaryZh":"不超过120字的中文总结"}]}。summaryZh 必须忠于 abstract，不得补充摘要中没有的信息；abstract 为空时 level、reason、summaryZh 都留空。只判断相关性，不判断质量。`},
		{Role: "user", Content: string(input)},
	}})
	if err != nil {
		// Candidates are still shown with their complete source abstracts.
		slog.Error("judge relevance and summarize abstracts failed; candidates will show complete abstracts without AI summaries", "candidates", len(candidates), "error", err)
		return result, 0
	}
	var response struct {
		Items []struct {
			Index     *int   `json:"index"`
			Level     string `json:"level"`
			Reason    string `json:"reason"`
			SummaryZH string `json:"summaryZh"`
		} `json:"items"`
	}
	if unmarshalErr := json.Unmarshal([]byte(stripCodeFence(content)), &response); unmarshalErr != nil {
		slog.Error("judge relevance and summarize abstracts returned unusable JSON", "error", unmarshalErr)
		return result, usage.CostUSD
	}
	seen := make(map[int]struct{})
	for _, raw := range response.Items {
		if raw.Index == nil || *raw.Index < 0 || *raw.Index >= len(candidates) {
			continue
		}
		index := *raw.Index
		if candidates[index].Abstract == nil || (raw.Level != "high" && raw.Level != "medium" && raw.Level != "low") {
			continue
		}
		if _, duplicate := seen[index]; duplicate {
			continue
		}
		seen[index] = struct{}{}
		result[index] = domain.RelevanceJudgment{
			Index: index, Level: raw.Level,
			Reason:    truncateRunes(strings.TrimSpace(raw.Reason), 60),
			SummaryZH: truncateRunes(strings.TrimSpace(raw.SummaryZH), 120),
		}
	}
	return result, usage.CostUSD
}

func normalizedAbstract(abstract *string) *string {
	if abstract == nil {
		return nil
	}
	value := strings.TrimSpace(*abstract)
	if value == "" {
		return nil
	}
	return &value
}

func sameSearchSpec(left, right domain.SearchSpec) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// quality returns the tier, its label and an optional caveat. Any matched venue rank puts the
// paper in the venue tier, and every matched rank system is listed together.
func quality(candidate domain.Candidate) (string, string, string) {
	if candidate.HasVenueRank() {
		return "venue", venueRankLabel(candidate), ""
	}
	note := ""
	if candidate.Source == domain.PaperSourceIEEE {
		// An unmatched venue is stated explicitly, never left for the reader to infer (ADR 0020).
		note = "发表场所等级：未匹配（CCF、JCR、中科院均未收录或无法确定）"
		if candidate.RankVersion == nil {
			note = "发表场所等级：等级数据暂不可用"
		}
	}
	if candidate.PublicationDate != nil && time.Since(*candidate.PublicationDate) < 60*24*time.Hour {
		return "too_new", "过新 · 暂无可靠引用信号", note
	}
	if !candidate.CitationKnown {
		return "citation_unknown", "引用数据暂未获取", note
	}
	return "citation_velocity", fmt.Sprintf("被引 %d 次", candidate.CitationCount), note
}

func venueRankLabel(candidate domain.Candidate) string {
	parts := make([]string, 0, 5)
	if candidate.CCFRank != nil {
		label := "CCF " + *candidate.CCFRank
		if candidate.CCFVenue != "" {
			label += "（" + candidate.CCFVenue + "）"
		}
		parts = append(parts, label)
	}
	if candidate.JCRQuartile != nil {
		parts = append(parts, "JCR "+*candidate.JCRQuartile)
	}
	if candidate.CASQuartile != nil {
		parts = append(parts, fmt.Sprintf("中科院 %d区", *candidate.CASQuartile))
	}
	if candidate.ImpactFactor != nil {
		parts = append(parts, "IF "+strconv.FormatFloat(*candidate.ImpactFactor, 'f', -1, 64))
	}
	if candidate.VenueRaw != nil && strings.TrimSpace(*candidate.VenueRaw) != "" {
		parts = append(parts, strings.TrimSpace(*candidate.VenueRaw))
	}
	return strings.Join(parts, " · ")
}

// searchSource picks the catalogue for this search from the user's literal message. An
// affirmative mention of exactly one source wins; otherwise continue/refine keep the
// direction's source and a new direction uses arXiv.
func searchSource(mode, userText string, previous domain.SearchSpec) string {
	lower := strings.ToLower(userText)
	mentionsIEEE, rejectsIEEE := sourceMention(lower, "ieee")
	mentionsArxiv, rejectsArxiv := sourceMention(lower, "arxiv")
	switch {
	case mentionsIEEE && !mentionsArxiv:
		return domain.PaperSourceIEEE
	case mentionsArxiv && !mentionsIEEE:
		return domain.PaperSourceArxiv
	case rejectsIEEE && !rejectsArxiv && !mentionsIEEE:
		return domain.PaperSourceArxiv
	case rejectsArxiv && !rejectsIEEE && !mentionsArxiv:
		return domain.PaperSourceIEEE
	case mode == "restart":
		return domain.PaperSourceArxiv
	default:
		return previous.EffectiveSource()
	}
}

// sourceNegations are phrases that, directly before a source name, mean "not that source".
var sourceNegations = []string{"不要", "不用", "不查", "别查", "别用", "除了", "不是", "排除", "not ", "no ", "without ", "exclude "}

// sourceMention reports whether name occurs with no negation phrase immediately before it
// (affirmative) and whether it occurs negated, so "不要 IEEE 的" rejects IEEE instead of
// selecting it.
func sourceMention(lower, name string) (affirmative, negated bool) {
	for offset := 0; ; {
		index := strings.Index(lower[offset:], name)
		if index < 0 {
			return affirmative, negated
		}
		start := offset + index
		before := strings.TrimRight(lower[:start], " ")
		isNegated := false
		for _, negation := range sourceNegations {
			if strings.HasSuffix(before, strings.TrimSpace(negation)) {
				isNegated = true
				break
			}
		}
		if isNegated {
			negated = true
		} else {
			affirmative = true
		}
		offset = start + len(name)
	}
}

// specSourceValue stores arXiv as the empty legacy value so pre-IEEE specs compare unchanged.
func specSourceValue(source string) string {
	if source == domain.PaperSourceIEEE {
		return domain.PaperSourceIEEE
	}
	return ""
}

func renderCandidates(conversation domain.Conversation, candidates []domain.Candidate, shown []domain.ShownPaper, requested, omitted int) string {
	byID := make(map[uint64]domain.Candidate, len(candidates))
	for _, item := range candidates {
		byID[item.ID] = item
	}
	var out strings.Builder
	out.WriteString(conversationHeader(conversation))
	out.WriteByte('\n')
	out.WriteString(renderSearchSummary(len(shown)+omitted, requested))
	out.WriteByte('\n')
	if omitted > 0 {
		fmt.Fprintf(&out, "受 QQ 单条消息长度限制，本次展示 %d 篇，另有 %d 篇未展示。\n", len(shown), omitted)
	}
	for _, item := range shown {
		paper := byID[item.PaperID]
		fmt.Fprintf(&out, "\n#%d %s\n", item.Ordinal, paper.Title)
		out.WriteString(item.QualityLabel)
		if item.Relevance != nil {
			fmt.Fprintf(&out, " · 相关性 %s", relevanceLabel(*item.Relevance))
		}
		out.WriteByte('\n')
		if item.QualityNote != "" {
			out.WriteString(item.QualityNote + "\n")
		}
		if item.SummaryZH != "" {
			out.WriteString(item.SummaryZH + "\n")
		} else if paper.Abstract == nil {
			out.WriteString("数据源未提供摘要，未让模型推测内容，也未判断相关性。\n")
		}
		if paper.TLDR != nil && strings.TrimSpace(*paper.TLDR) != "" {
			fmt.Fprintf(&out, "论文速览：%s\n", strings.TrimSpace(*paper.TLDR))
		}
		if item.ArxivID != "" {
			fmt.Fprintf(&out, "arXiv: %s\n", item.ArxivID)
		}
		if item.Source == domain.PaperSourceIEEE && item.LandingURL != "" {
			fmt.Fprintf(&out, "IEEE DOI: %s\n论文链接：%s\n", item.DOI, item.LandingURL)
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// renderSearchSummary states what a search produced in the user's terms. It is the whole Web
// bubble (the cards carry per-paper detail) and the first line of the QQ list. The requested
// count is the tool argument, which defaults when the user named no number, so it is never
// quoted back; it only decides whether to say that fewer papers were found (CONTEXT.md 候选列表).
func renderSearchSummary(found, requested int) string {
	if found < requested {
		return fmt.Sprintf("只找到 %d 篇符合条件的候选论文，按相关性排列。可以放宽条件或换个说法再找。", found)
	}
	return fmt.Sprintf("找到 %d 篇候选论文，按相关性排列。", found)
}

func relevanceLabel(level string) string {
	switch level {
	case "high":
		return "高"
	case "medium":
		return "中"
	case "low":
		return "低"
	default:
		return level
	}
}

// fitCandidateResponse picks the papers that fit the QQ single-message budget and returns them
// with the QQ text. Whole low-relevance candidates are dropped from the end, never truncated.
func fitCandidateResponse(conversation domain.Conversation, candidates []domain.Candidate, shown []domain.ShownPaper, requested, maxRunes int) ([]domain.ShownPaper, string) {
	visible := shown
	for {
		omitted := len(shown) - len(visible)
		response := renderCandidates(conversation, candidates, visible, requested, omitted)
		if len([]rune(response)) <= maxRunes {
			return visible, response
		}
		if len(visible) <= 1 {
			paper := candidates[0]
			if len(visible) == 1 {
				for _, candidate := range candidates {
					if candidate.ID == visible[0].PaperID {
						paper = candidate
						break
					}
				}
			}
			slog.Warn("single candidate exceeds the QQ search response budget", "runes", len([]rune(response)), "budget", maxRunes, "paper_id", paper.ID)
			message := fmt.Sprintf("%s\n候选论文《%s》的内容超过 QQ 单条消息可安全承载的长度，本次没有截断发送。请缩小检索范围后重试。", conversationHeader(conversation), paper.Title)
			return nil, message
		}
		visible = visible[:len(visible)-1]
	}
}

func relevanceScore(level *string) int {
	if level == nil {
		return 0
	}
	switch *level {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func stripCodeFence(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "```json")
	value = strings.TrimPrefix(value, "```")
	value = strings.TrimSuffix(value, "```")
	return strings.TrimSpace(value)
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}
