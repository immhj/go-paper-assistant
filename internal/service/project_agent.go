package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

const (
	projectAgentOutputRunes   = 12_000
	projectHistoryEntryRunes  = 2_500
	projectEvidenceRunes      = 80_000
	projectPaperEvidenceRunes = 16_000
)

type projectAgentContextKey struct{}

type projectAgentScope struct {
	Owner     domain.ProjectOwner
	ProjectNo uint32
}

func withProjectAgentScope(ctx context.Context, owner domain.ProjectOwner, projectNo uint32) context.Context {
	return context.WithValue(ctx, projectAgentContextKey{}, projectAgentScope{Owner: owner, ProjectNo: projectNo})
}

func projectAgentScopeFromContext(ctx context.Context) (projectAgentScope, bool) {
	scope, ok := ctx.Value(projectAgentContextKey{}).(projectAgentScope)
	if !ok || scope.ProjectNo == 0 || scope.Owner.Validate() != nil {
		return projectAgentScope{}, false
	}
	return scope, true
}

func (a *Application) listCurrentProjectPapers(ctx context.Context, conversation domain.Conversation, limit int, deliver deliverer) error {
	scope, ok := projectAgentScopeFromContext(ctx)
	if !ok {
		return errors.New("current project scope is unavailable")
	}
	items, err := a.store.ListCurrentProjectPapers(ctx, scope.Owner, scope.ProjectNo, nil, limit)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return deliver.Text(ctx, conversation, "当前项目还没有论文。可以在检索结果里展开论文，点击“加入项目”。", nil, 0)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "当前项目 #%d 的论文（最多 %d 篇）：\n", scope.ProjectNo, limit)
	for _, item := range items {
		fmt.Fprintf(&out, "- 论文 ID %d｜%s", item.PaperID, boundedRunes(item.Title, 300, "…"))
		if item.LatestReadResult != nil {
			fmt.Fprintf(&out, "｜最近%s：会话 #%d，%s", readTypeLabel(item.LatestReadResult.ReadType), item.LatestReadResult.SourceConversationNo, item.LatestReadResult.CreatedAt.Format("2006-01-02 15:04"))
		}
		out.WriteByte('\n')
	}
	out.WriteString("\n精读记录是过去生成的分析，可能与原文有出入，引用前请以论文原文为准。")
	return deliver.Stream(ctx, conversation, boundedRunes(out.String(), projectAgentOutputRunes, "\n[输出已截断]"), 0)
}

func (a *Application) getCurrentProjectPaperReadHistory(ctx context.Context, conversation domain.Conversation, paperID uint64, limit int, deliver deliverer) error {
	scope, ok := projectAgentScopeFromContext(ctx)
	if !ok {
		return errors.New("current project scope is unavailable")
	}
	papers, err := a.store.ListCurrentProjectPapers(ctx, scope.Owner, scope.ProjectNo, []uint64{paperID}, 1)
	if err != nil {
		return err
	}
	if len(papers) != 1 || papers[0].PaperID != paperID {
		return deliver.Text(ctx, conversation, "当前项目中没有这篇论文。", nil, 0)
	}
	items, _, err := a.store.ListProjectReadResults(ctx, scope.Owner, scope.ProjectNo, paperID, limit, nil)
	if errors.Is(err, dao.ErrProjectPaperNotFound) {
		return deliver.Text(ctx, conversation, "当前项目中没有这篇论文。", nil, 0)
	}
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return deliver.Text(ctx, conversation, fmt.Sprintf("论文 ID %d 还没有精读记录。", paperID), nil, 0)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "论文 ID %d《%s》的精读历史（最多 %d 条）：\n", paperID, boundedRunes(papers[0].Title, 300, "…"), limit)
	for index, item := range items {
		fmt.Fprintf(&out, "\n%d. %s｜来源会话 #%d｜%s\n（过去生成的分析，可能与原文有出入，引用前请以原文为准）\n%s\n", index+1, readTypeLabel(item.ReadType), item.SourceConversationNo, item.CreatedAt.Format("2006-01-02 15:04"), boundedRunes(item.Content, projectHistoryEntryRunes, "\n[本条历史输出已截断]"))
		if len([]rune(out.String())) >= projectAgentOutputRunes {
			break
		}
	}
	return deliver.Stream(ctx, conversation, boundedRunes(out.String(), projectAgentOutputRunes, "\n[历史列表输出已截断]"), 0)
}

func (a *Application) answerFromCurrentProjectPapers(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, paperIDs []uint64, question string, deliver deliverer) error {
	scope, ok := projectAgentScopeFromContext(ctx)
	if !ok {
		return errors.New("current project scope is unavailable")
	}
	papers, err := a.store.ListCurrentProjectPapers(ctx, scope.Owner, scope.ProjectNo, paperIDs, len(paperIDs))
	if err != nil {
		return err
	}
	byID := make(map[uint64]domain.ProjectPaper, len(papers))
	for _, paper := range papers {
		byID[paper.PaperID] = paper
	}
	if len(byID) != len(paperIDs) {
		return deliver.Text(ctx, conversation, "所选论文中至少有一篇不在当前项目中。", nil, 0)
	}

	var evidence strings.Builder
	usable := 0
	perPaperBudget := (projectEvidenceRunes - len(paperIDs)*500) / len(paperIDs)
	if perPaperBudget > projectPaperEvidenceRunes {
		perPaperBudget = projectPaperEvidenceRunes
	}
	if perPaperBudget < 1_000 {
		perPaperBudget = 1_000
	}
	for _, paperID := range paperIDs {
		relation, exists := byID[paperID]
		if !exists {
			return deliver.Text(ctx, conversation, "所选论文中至少有一篇不在当前项目中。", nil, 0)
		}
		paper, paperErr := a.store.GetPaper(ctx, paperID)
		if paperErr != nil {
			return paperErr
		}
		fmt.Fprintf(&evidence, "\n\n=== 论文 ID %d %s ===\n", paperID, boundedRunes(relation.Title, 300, "…"))
		body, source := verifiedProjectPaperEvidence(paper)
		// The account's own private full text is verified evidence for that account only.
		if private, ok, privateErr := a.accountPrivateFullText(ctx, paperID); privateErr != nil {
			return privateErr
		} else if ok && strings.TrimSpace(private) != "" {
			body, source = strings.TrimSpace(private), "账号私有全文（已校验）"
		}
		if body == "" {
			evidence.WriteString("[无可核对的摘要或已校验正文；该论文不可用于事实结论]")
			continue
		}
		usable++
		fmt.Fprintf(&evidence, "证据来源：%s\n%s", source, boundedRunes(body, perPaperBudget, "\n[单篇证据已截断]"))
	}
	if usable == 0 {
		return deliver.Text(ctx, conversation, "所选论文都没有可用的摘要或全文，暂时无法据此回答。可以先为这些论文上传全文或换几篇论文。", nil, 0)
	}

	request := deepseek.Request{Thinking: true, Messages: []deepseek.Message{
		{Role: "system", Content: "你是严谨的项目论文研究助手。只能依据本次重新读取的论文摘要或已校验正文回答；不得把旧精读历史当作证据。逐篇用“论文 ID N”标明出处，不要提及内部字段名，区分论文明确陈述与分析推断；缺少证据必须明确说明，不能补写。"},
		{Role: "user", Content: "问题：" + strings.TrimSpace(question) + "\n当前项目论文证据：" + boundedRunes(evidence.String(), projectEvidenceRunes, "\n[总证据已截断]")},
	}}
	if usage, streamed, streamErr := a.streamModelToWeb(ctx, conversation, request, 0, "", "", "", deliver); streamed {
		a.recordToolUsage(input.MessageID, usage)
		if streamErr != nil {
			return errors.Join(streamErr, deliver.Text(ctx, conversation, "基于当前项目论文的回答失败，请稍后重试。", nil, 0))
		}
		return nil
	}

	content, usage, err := a.model.Complete(ctx, request)
	a.recordToolUsage(input.MessageID, usage)
	if err != nil {
		return errors.Join(err, deliver.Text(ctx, conversation, "基于当前项目论文的回答失败，请稍后重试。", nil, 0))
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.Join(errors.New("project research model returned empty content"), deliver.Text(ctx, conversation, "基于当前项目论文的回答没有生成内容，请稍后重试。", nil, 0))
	}
	return deliver.Stream(ctx, conversation, limitReadOutput(content), usage.CostUSD)
}

// readTypeLabel turns the stored read type into the user's term (CONTEXT.md 精读).
func readTypeLabel(readType string) string {
	switch readType {
	case domain.ProjectReadTypeQuick:
		return "快速精读"
	case domain.ProjectReadTypeDeep:
		return "深度精读"
	default:
		return "精读"
	}
}

func verifiedProjectPaperEvidence(paper domain.Paper) (string, string) {
	if paper.FullTextStatus == "ok" && paper.FullTextPath != nil && strings.TrimSpace(*paper.FullTextPath) != "" {
		content, err := os.ReadFile(*paper.FullTextPath)
		if err == nil && strings.TrimSpace(string(content)) != "" {
			return strings.TrimSpace(string(content)), "已校验正文"
		}
	}
	if paper.Abstract != nil && strings.TrimSpace(*paper.Abstract) != "" {
		return strings.TrimSpace(*paper.Abstract), "论文摘要"
	}
	return "", ""
}

func distinctPaperIDs(values []uint64, minimum, maximum int) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		if value == 0 {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func boundedRunes(value string, limit int, suffix string) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + suffix
}
