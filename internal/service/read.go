package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

const (
	quickReadInputRunes = 24_000
	readOutputRunes     = 10_000
)

func (a *Application) readPaper(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, argument, readType string, deliver deliverer) error {
	deep := readType == domain.ProjectReadTypeDeep
	if readType != domain.ProjectReadTypeQuick && readType != domain.ProjectReadTypeDeep {
		return errors.New("invalid paper read type")
	}
	ordinal, err := parsePositiveNumber(argument)
	if err != nil {
		example := "快速精读第3篇"
		if deep {
			example = "深度精读第3篇"
		}
		// /read was removed; business actions are natural language (agents.go routeTurn).
		return deliver.Text(ctx, conversation, fmt.Sprintf("请说明要精读第几篇，例如“%s”。", example), nil, 0)
	}
	unlock := a.lockConversation(conversation.ID)
	defer unlock()
	current := parseSearchContext(conversation.CurrentContext, conversation.Title)
	paperID, ok := ordinalPaperID(current, ordinal)
	if !ok {
		return deliver.Text(ctx, conversation, fmt.Sprintf("当前会话里没有论文 #%d。请先检索或检查序号。", ordinal), nil, 0)
	}
	paper, err := a.store.GetPaper(ctx, paperID)
	if err != nil {
		return err
	}
	modeLabel := "快速精读"
	progress := "将读取全文重点片段，完成后可以继续深度精读。"
	if deep {
		modeLabel = "深度精读"
		progress = "将基于完整全文深入分析，耗时会比快速精读更长。"
	}
	if err = announce(ctx, deliver, conversation, "read.prepare",
		fmt.Sprintf("正在%s #%d：%s", modeLabel, ordinal, progress),
		fmt.Sprintf("%s\n正在%s #%d《%s》……\n%s", conversationHeader(conversation), modeLabel, ordinal, paper.Title, progress)); err != nil {
		return err
	}

	cached := paper.FullTextStatus == "ok" && paper.FullTextPath != nil
	fullTextStarted := time.Now()
	resolved, err := a.fullText(ctx, paper)
	var unavailable *fullTextUnavailableError
	if errors.As(err, &unavailable) {
		// Missing full text is an expected state with a user action, not a failed task.
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+a.fullTextUnavailableMessage(ctx, unavailable), nil, 0)
	}
	if err != nil {
		// The cause (download/extraction/storage detail) is logged, never shown to the user.
		slog.Error("paper full text failed", "paper_id", paper.ID, "deep", deep, "error", err)
		message := "暂时无法获取这篇论文的全文，请稍后重试。"
		if link := paper.LandingURL(); link != "" {
			message += "\n论文链接：" + link
		}
		return errors.Join(err, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+message, nil, 0))
	}
	slog.Info("paper full text ready", "paper_id", paper.ID, "deep", deep, "cached", cached, "private", resolved.private, "runes", len([]rune(resolved.text)), "duration", time.Since(fullTextStarted))
	if !a.model.Available() {
		slog.Error("paper read skipped: model is not configured", "paper_id", paper.ID)
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n全文已准备好，但精读服务暂时不可用，请稍后重试。", nil, 0)
	}
	if err = a.markSharedFullText(ctx, paper, resolved); err != nil {
		return err
	}
	return a.completeRead(ctx, conversation, input, ordinal, paper.ID, readType, paper, resolved.text, deliver)
}

// resolvedFullText is verified full text plus where it came from. Private text has no shared
// cache path and must never be written into papers.fulltext_path.
type resolvedFullText struct {
	text    string
	path    string
	private bool
}

// fullTextUnavailableError means no verified full text exists that this actor may use. The
// user can resolve it by downloading the PDF with their own access and uploading it.
type fullTextUnavailableError struct {
	paper  domain.Paper
	reason string
}

func (e *fullTextUnavailableError) Error() string { return e.reason }

// fullText resolves full text in the order agreed in ADR 0019: the current account's private
// full text, then the shared open-access cache, then a fresh open-access download.
func (a *Application) fullText(ctx context.Context, paper domain.Paper) (resolvedFullText, error) {
	if text, ok, err := a.accountPrivateFullText(ctx, paper.ID); err != nil {
		return resolvedFullText{}, err
	} else if ok {
		return resolvedFullText{text: text, private: true}, nil
	}
	if paper.FullTextStatus == "ok" && paper.FullTextPath != nil {
		content, err := os.ReadFile(*paper.FullTextPath)
		if err == nil {
			return resolvedFullText{text: string(content), path: *paper.FullTextPath}, nil
		}
	}
	if paper.Source == "" || paper.ExternalID == "" {
		return resolvedFullText{}, errors.New("论文缺少来源标识，无法定位开放全文")
	}
	candidate, err := a.papers.GetByID(ctx, paper.Source, paper.ExternalID)
	if err != nil {
		if paper.Source == domain.PaperSourceIEEE {
			// The OpenAlex refresh only looks for an open-access copy; when it is unavailable the
			// user can still upload their own PDF, so this is a prompt, not a failed task.
			slog.Warn("IEEE open-access lookup failed; asking for upload", "paper_id", paper.ID, "error", err)
			return resolvedFullText{}, &fullTextUnavailableError{paper: paper, reason: "暂时无法查询开放 PDF"}
		}
		return resolvedFullText{}, fmt.Errorf("刷新论文信息失败: %w", err)
	}
	if candidate.OpenAccessPDFURL == "" {
		return resolvedFullText{}, &fullTextUnavailableError{paper: paper, reason: "这篇论文没有可用的开放 PDF"}
	}
	path, text, err := a.fulltext.Fetch(ctx, paper.ID, candidate.OpenAccessPDFURL)
	if err != nil {
		// A non-empty extracted body means download/parser succeeded but validation failed.
		// Persist that state so the next /read does not silently treat this as a healthy cache.
		if text != "" {
			if persistErr := a.store.UpdatePaperFullText(ctx, paper.ID, "", "bad"); persistErr != nil {
				return resolvedFullText{}, fmt.Errorf("%w; 保存全文校验状态失败: %v", err, persistErr)
			}
		}
		if paper.Source == domain.PaperSourceIEEE {
			// Publisher sites often refuse server-side downloads; fall back to user upload.
			slog.Warn("open-access IEEE PDF download failed; asking for upload", "paper_id", paper.ID, "error", err)
			return resolvedFullText{}, &fullTextUnavailableError{paper: paper, reason: "开放 PDF 下载或校验失败"}
		}
		return resolvedFullText{path: path}, err
	}
	return resolvedFullText{text: text, path: path}, nil
}

// markSharedFullText records a newly verified shared cache entry. Private text is skipped.
func (a *Application) markSharedFullText(ctx context.Context, paper domain.Paper, resolved resolvedFullText) error {
	if resolved.private || paper.FullTextStatus == "ok" {
		return nil
	}
	if err := a.store.UpdatePaperFullText(ctx, paper.ID, resolved.path, "ok"); err != nil {
		return fmt.Errorf("保存全文状态失败: %w", err)
	}
	return nil
}

// currentAccountID returns the Web account behind this turn, if any. Guests and QQ users
// have no private full text (ADR 0019).
func currentAccountID(ctx context.Context) (uint64, bool) {
	scope, ok := projectAgentScopeFromContext(ctx)
	if !ok || !scope.Owner.IsAccount() {
		return 0, false
	}
	return scope.Owner.ID, true
}

func (a *Application) accountPrivateFullText(ctx context.Context, paperID uint64) (string, bool, error) {
	accountID, ok := currentAccountID(ctx)
	if !ok || paperID == 0 {
		return "", false, nil
	}
	text, err := a.store.GetAccountFullText(ctx, accountID, paperID)
	if errors.Is(err, dao.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取私有全文失败: %w", err)
	}
	return text, true, nil
}

func (a *Application) fullTextUnavailableMessage(ctx context.Context, unavailable *fullTextUnavailableError) string {
	var out strings.Builder
	fmt.Fprintf(&out, "《%s》%s，暂时无法精读。", unavailable.paper.Title, unavailable.reason)
	if link := unavailable.paper.LandingURL(); link != "" {
		fmt.Fprintf(&out, "\n论文链接：%s", link)
	}
	if _, ok := currentAccountID(ctx); ok {
		out.WriteString("\n请用你自己的机构权限下载 PDF，然后在这篇论文的卡片上点击“上传全文 PDF”，校验通过后再发起精读。")
	} else {
		out.WriteString("\n订阅论文需要先登录 Web 账号，用你自己的机构权限下载 PDF 后在论文卡片上上传全文，再发起精读。")
	}
	return out.String()
}

func (a *Application) completeRead(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, ordinal int, paperID uint64, readType string, paper domain.Paper, fullText string, deliver deliverer) error {
	if paperID == 0 || paper.ID != paperID || (readType != domain.ProjectReadTypeQuick && readType != domain.ProjectReadTypeDeep) {
		return errors.New("invalid paper read identity")
	}
	deep := readType == domain.ProjectReadTypeDeep
	modeLabel := "快速精读"
	modelInput := quickReadExcerpt(fullText)
	request := deepseek.Request{Messages: []deepseek.Message{
		{Role: "system", Content: `你是严谨的论文快速精读助手。输入是从已校验全文中选出的重点片段，不一定覆盖所有章节。只依据片段输出：研究问题、核心方法、关键实验结果、主要局限、是否值得进行深度精读。明确标注片段未覆盖、不能确认的信息。控制在 1200 字以内。`},
		{Role: "user", Content: "用户当前研究方向：" + parseSearchContext(conversation.CurrentContext, conversation.Title).Name + "\n论文全文重点片段：\n" + modelInput},
	}}
	if deep {
		modeLabel = "深度精读"
		request = deepseek.Request{Thinking: true, Messages: []deepseek.Message{
			{Role: "system", Content: `你是严谨的论文深度精读助手。只依据给定完整全文回答，区分“论文明确陈述”和“你的分析推断”；全文未涉及的信息必须说“论文中未涉及”。输出：研究问题、核心方法、实验设置、baseline、主要结果、消融实验、局限性、适合什么需求。控制在 3000 字以内。`},
			{Role: "user", Content: "用户当前研究方向：" + parseSearchContext(conversation.CurrentContext, conversation.Title).Name + "\n论文完整全文：\n" + fullText},
		}}
	}
	modelStarted := time.Now()
	modelInputRunes := len([]rune(modelInput))
	if deep {
		modelInputRunes = len([]rune(fullText))
	}
	header := fmt.Sprintf("%s\n#%d《%s》\n%s\n\n", conversationHeader(conversation), ordinal, paper.Title, modeLabel)
	suffix := "\n\n已完成深度精读（基于完整全文）。"
	if !deep {
		suffix = fmt.Sprintf("\n\n以上是快速精读，只覆盖全文重点片段。需要完整实验设置、baseline、消融实验和局限分析时，可以直接说‘深度精读第%d篇’。", ordinal)
	}
	if usage, streamed, streamErr := a.streamModelToWeb(ctx, conversation, request, paperID, readType, header, suffix, deliver); streamed {
		a.recordToolUsage(input.MessageID, usage)
		if streamErr != nil {
			slog.Error("paper read model stream failed", "paper_id", paper.ID, "ordinal", ordinal, "deep", deep, "duration", time.Since(modelStarted), "error", streamErr)
			return errors.Join(streamErr, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+readFailureMessage(streamErr), nil, 0))
		}
		slog.Info("paper read model streamed", "paper_id", paper.ID, "ordinal", ordinal, "deep", deep, "input_runes", modelInputRunes, "duration", time.Since(modelStarted), "cost_usd", usage.CostUSD)
		markStepSucceeded(ctx)
		return nil
	}

	content, usage, err := a.model.Complete(ctx, request)
	a.recordToolUsage(input.MessageID, usage)
	if err != nil {
		slog.Error("paper read model failed", "paper_id", paper.ID, "ordinal", ordinal, "deep", deep, "duration", time.Since(modelStarted), "error", err)
		return errors.Join(err, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+readFailureMessage(err), nil, 0))
	}
	slog.Info("paper read model completed", "paper_id", paper.ID, "ordinal", ordinal, "deep", deep, "input_runes", modelInputRunes, "duration", time.Since(modelStarted), "cost_usd", usage.CostUSD)
	content = strings.TrimSpace(content)
	if content == "" {
		emptyErr := errors.New("model returned empty read content")
		return errors.Join(emptyErr, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n模型没有返回精读内容，请稍后重试。", nil, 0))
	}
	finalText := header + limitReadOutput(content) + suffix
	if err = deliver.Read(ctx, conversation, paperID, readType, finalText, usage.CostUSD); err != nil {
		return err
	}
	markStepSucceeded(ctx)
	return nil
}

func (a *Application) deliverReadResult(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, finalText string, cost float64) error {
	count, err := a.store.CountBotReplies(ctx, input.MessageID)
	if err != nil {
		return err
	}
	if count >= 4 {
		return errors.New("QQ passive reply quota exhausted")
	}
	seq := uint32(count + 1)
	initial := streamPrefix(finalText, 180)
	qqID := input.MessageID
	reserved, err := a.store.InsertMessage(ctx, domain.Message{
		ConversationID: conversation.ID, Role: "bot", QQMessageID: &qqID,
		MessageSeq: &seq, Content: &initial,
	})
	if err != nil {
		return err
	}
	first, err := a.messenger.SendStream(ctx, input.OpenID, qq.StreamChunk{
		MessageID: input.MessageID, MessageSeq: seq, Index: 0, Content: initial,
	})
	if err != nil {
		failure := conversationHeader(conversation) + "\n精读结果已生成，但 QQ 流式消息启动失败，请重新发送精读命令。"
		_ = a.store.UpdateMessageContent(ctx, reserved.ID, failure, 0)
		_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "failed", "")
		return errors.Join(err, a.replyText(ctx, conversation, input, failure, nil, 0))
	}
	_, err = a.messenger.SendStream(ctx, input.OpenID, qq.StreamChunk{
		MessageID: input.MessageID, MessageSeq: seq, StreamID: first.ID,
		Index: 1, Content: finalText, Finished: true,
	})
	if err == nil {
		if updateErr := a.store.UpdateMessageContent(ctx, reserved.ID, finalText, cost); updateErr != nil {
			return updateErr
		}
		return a.store.UpdateMessageDelivery(ctx, reserved.ID, "sent", first.ID)
	}

	failure := conversationHeader(conversation) + "\n精读结果已生成，但 QQ 提交最终消息失败，请重新发送精读命令。"
	_ = a.store.UpdateMessageContent(ctx, reserved.ID, failure, 0)
	_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "failed", first.ID)
	// QQ replace streams require every accepted snapshot to keep the exact
	// previously submitted prefix. Close with the accepted prefix, never replace
	// it with an unrelated failure sentence; report the failure separately.
	_, finishErr := a.messenger.SendStream(ctx, input.OpenID, qq.StreamChunk{
		MessageID: input.MessageID, MessageSeq: seq, StreamID: first.ID,
		Index: 2, Content: initial, Finished: true,
	})
	replyErr := a.replyText(ctx, conversation, input, failure, nil, 0)
	return errors.Join(err, finishErr, replyErr)
}

func quickReadExcerpt(fullText string) string {
	runes := []rune(fullText)
	if len(runes) <= quickReadInputRunes {
		return fullText
	}
	const headRunes = 16_000
	const tailRunes = quickReadInputRunes - headRunes
	tailStart := len(runes) - tailRunes
	markerAt := -1
	for _, marker := range []string{"\nConclusions", "\nCONCLUSIONS", "\nconclusions", "\nConclusion", "\nCONCLUSION", "\nconclusion", "\n结论"} {
		if index := strings.LastIndex(fullText, marker); index > markerAt {
			markerAt = index
		}
	}
	if markerAt >= 0 {
		tailStart = len([]rune(fullText[:markerAt]))
		if tailStart < headRunes {
			tailStart = len(runes) - tailRunes
		}
	}
	tailEnd := tailStart + tailRunes
	if tailEnd > len(runes) {
		tailEnd = len(runes)
		tailStart = tailEnd - tailRunes
	}
	return string(runes[:headRunes]) + "\n\n[中间部分在快速精读中省略；深度精读会使用完整全文]\n\n" + string(runes[tailStart:tailEnd])
}

func limitReadOutput(content string) string {
	runes := []rune(content)
	if len(runes) <= readOutputRunes {
		return content
	}
	return string(runes[:readOutputRunes]) + "\n\n[达到 QQ 精读结果长度上限，已停止输出]"
}

func readFailureMessage(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "insufficient balance"), strings.Contains(message, "status 402"):
		// An operator problem (provider balance): the user cannot act on it, so it is logged
		// for the operator and the user only learns the service is temporarily unavailable.
		slog.Error("model provider balance exhausted", "error", err)
		return "精读失败：精读服务暂时不可用，请稍后重试。"
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline exceeded"):
		return "精读失败：模型处理超时。可以稍后重新说‘快速精读第N篇’或‘深度精读第N篇’。"
	default:
		return "精读失败：模型服务暂时不可用，请稍后重试。"
	}
}

func decodeShown(raw json.RawMessage) []domain.ShownPaper {
	var items []domain.ShownPaper
	_ = json.Unmarshal(raw, &items)
	return items
}
