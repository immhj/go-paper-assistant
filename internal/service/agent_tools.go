package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

type ordinalPaper struct {
	Ordinal int
	Paper   domain.Paper
}

func (a *Application) currentPapers(ctx context.Context, conversation domain.Conversation, selected []int) ([]ordinalPaper, error) {
	current := parseSearchContext(conversation.CurrentContext, conversation.Title)
	filter := make(map[int]struct{}, len(selected))
	for _, ordinal := range selected {
		if ordinal > 0 {
			filter[ordinal] = struct{}{}
		}
	}
	items := make([]ordinalPaper, 0, len(current.Ordinals))
	for paperID, ordinal := range current.Ordinals {
		if len(filter) > 0 {
			if _, ok := filter[ordinal]; !ok {
				continue
			}
		}
		id, ok := ordinalPaperID(current, ordinal)
		if !ok || fmt.Sprintf("%d", id) != paperID {
			return nil, fmt.Errorf("invalid paper ordinal mapping #%d", ordinal)
		}
		paper, err := a.store.GetPaper(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, ordinalPaper{Ordinal: ordinal, Paper: paper})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Ordinal < items[j].Ordinal })
	if len(filter) > 0 && len(items) != len(filter) {
		return nil, daoNotFound("one or more paper ordinals are not in the current conversation")
	}
	return items, nil
}

type domainNotFoundError string

func (e domainNotFoundError) Error() string { return string(e) }
func daoNotFound(message string) error      { return domainNotFoundError(message) }

func (a *Application) listCurrentPapers(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, deliver deliverer) error {
	items, err := a.currentPapers(ctx, conversation, nil)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n当前会话还没有展示过论文。", nil, 0)
	}
	var out strings.Builder
	out.WriteString(conversationHeader(conversation))
	out.WriteString("\n当前会话论文：\n")
	for _, item := range items {
		fmt.Fprintf(&out, "#%d %s\n", item.Ordinal, item.Paper.Title)
	}
	return deliver.Text(ctx, conversation, out.String(), nil, 0)
}

func (a *Application) getPaperAbstract(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, ordinal int, deliver deliverer) error {
	items, err := a.currentPapers(ctx, conversation, []int{ordinal})
	if err != nil || len(items) != 1 {
		return deliver.Text(ctx, conversation, fmt.Sprintf("当前会话里没有论文 #%d。", ordinal), nil, 0)
	}
	paper := items[0].Paper
	abstract := "缺少摘要，未让模型推测内容。"
	if paper.Abstract != nil && strings.TrimSpace(*paper.Abstract) != "" {
		abstract = strings.TrimSpace(*paper.Abstract)
	}
	text := fmt.Sprintf("%s\n#%d《%s》\nAbstract：\n%s", conversationHeader(conversation), ordinal, paper.Title, abstract)
	if err = deliver.Stream(ctx, conversation, text, 0); err != nil {
		return err
	}
	markStepSucceeded(ctx)
	return nil
}

func (a *Application) comparePapers(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, ordinals []int, deliver deliverer) error {
	items, err := a.currentPapers(ctx, conversation, ordinals)
	if err != nil {
		return deliver.Text(ctx, conversation, "无法唯一解析要比较的论文，请给出当前会话中的2到3个论文序号。", nil, 0)
	}
	if err = announce(ctx, deliver, conversation, "compare.prepare", "正在读取论文并比较……", conversationHeader(conversation)+"\n正在读取全文并比较论文……"); err != nil {
		return err
	}
	var evidence strings.Builder
	for _, item := range items {
		resolved, textErr := a.fullText(ctx, item.Paper)
		var unavailable *fullTextUnavailableError
		if errors.As(textErr, &unavailable) {
			return deliver.Text(ctx, conversation, fmt.Sprintf("#%d %s", item.Ordinal, a.fullTextUnavailableMessage(ctx, unavailable)), nil, 0)
		}
		if textErr != nil {
			return deliver.Text(ctx, conversation, fmt.Sprintf("无法读取 #%d 全文：%v", item.Ordinal, textErr), nil, 0)
		}
		if textErr = a.markSharedFullText(ctx, item.Paper, resolved); textErr != nil {
			return textErr
		}
		fmt.Fprintf(&evidence, "\n\n=== #%d %s ===\n%s", item.Ordinal, item.Paper.Title, resolved.text)
	}
	request := deepseek.Request{Thinking: true, Messages: []deepseek.Message{
		{Role: "system", Content: "你是严谨的论文对比助手。只依据给定完整全文，逐项比较研究问题、方法、数据、baseline、主要结果、局限和适用场景；证据不足必须明确说明。"},
		{Role: "user", Content: evidence.String()},
	}}
	header := conversationHeader(conversation) + "\n论文对比\n\n"
	if usage, streamed, streamErr := a.streamModelToWeb(ctx, conversation, request, 0, "", header, "", deliver); streamed {
		a.recordToolUsage(input.MessageID, usage)
		if streamErr != nil {
			return errors.Join(streamErr, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n论文比较失败，请稍后重试。", nil, 0))
		}
		markStepSucceeded(ctx)
		return nil
	}
	content, usage, err := a.model.Complete(ctx, request)
	a.recordToolUsage(input.MessageID, usage)
	if err != nil {
		return errors.Join(err, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n论文比较失败，请稍后重试。", nil, 0))
	}
	content = strings.TrimSpace(content)
	if content == "" {
		emptyErr := errors.New("comparison model returned empty content")
		return errors.Join(emptyErr, deliver.Text(ctx, conversation, "论文比较没有生成内容，请稍后重试。", nil, 0))
	}
	if err = deliver.Stream(ctx, conversation, header+limitReadOutput(content), usage.CostUSD); err != nil {
		return err
	}
	markStepSucceeded(ctx)
	return nil
}

func (a *Application) answerResearchQuestion(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, question string, ordinals []int, deliver deliverer) error {
	items, err := a.currentPapers(ctx, conversation, ordinals)
	if err != nil {
		return deliver.Text(ctx, conversation, "无法解析问题所指的论文，请明确论文序号。", nil, 0)
	}
	if len(items) == 0 {
		return deliver.Text(ctx, conversation, "当前会话还没有论文依据，请先检索论文。", nil, 0)
	}
	var evidence strings.Builder
	for _, item := range items {
		fmt.Fprintf(&evidence, "\n#%d %s\n", item.Ordinal, item.Paper.Title)
		if item.Paper.Abstract != nil {
			evidence.WriteString(*item.Paper.Abstract)
		} else {
			evidence.WriteString("[缺少摘要]")
		}
	}
	request := deepseek.Request{Messages: []deepseek.Message{
		{Role: "system", Content: "只依据给定论文标题和摘要回答研究问题。区分论文明确陈述与推断；证据不足时必须说无法确定。"},
		{Role: "user", Content: "问题：" + question + "\n论文依据：" + evidence.String()},
	}}
	header := conversationHeader(conversation) + "\n\n"
	if usage, streamed, streamErr := a.streamModelToWeb(ctx, conversation, request, 0, "", header, "", deliver); streamed {
		a.recordToolUsage(input.MessageID, usage)
		if streamErr != nil {
			return errors.Join(streamErr, deliver.Text(ctx, conversation, "研究问题回答失败，请稍后重试。", nil, 0))
		}
		markStepSucceeded(ctx)
		return nil
	}
	content, usage, err := a.model.Complete(ctx, request)
	a.recordToolUsage(input.MessageID, usage)
	if err != nil {
		return errors.Join(err, deliver.Text(ctx, conversation, "研究问题回答失败，请稍后重试。", nil, 0))
	}
	content = strings.TrimSpace(content)
	if content == "" {
		emptyErr := errors.New("research model returned empty content")
		return errors.Join(emptyErr, deliver.Text(ctx, conversation, "研究问题没有生成回答，请稍后重试。", nil, 0))
	}
	if err = deliver.Stream(ctx, conversation, header+limitReadOutput(content), usage.CostUSD); err != nil {
		return err
	}
	markStepSucceeded(ctx)
	return nil
}

func (a *Application) replyGeneratedText(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, text string, cost float64) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("generated reply is empty")
	}
	count, err := a.store.CountBotReplies(ctx, input.MessageID)
	if err != nil {
		return err
	}
	if count >= 4 {
		return errors.New("QQ passive reply quota exhausted")
	}
	seq := uint32(count + 1)
	qqID := input.MessageID
	reserved, err := a.store.InsertMessage(ctx, domain.Message{ConversationID: conversation.ID, Role: "bot", QQMessageID: &qqID, MessageSeq: &seq, Content: &text, CostUSD: 0, DeliveryStatus: "reserved"})
	if err != nil {
		return err
	}
	outboundID, sendErr := a.sendFinalStreamWithSeq(ctx, input, seq, text)
	if sendErr != nil {
		_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "failed", outboundID)
		return sendErr
	}
	if updateErr := a.store.UpdateMessageContent(ctx, reserved.ID, text, cost); updateErr != nil {
		return updateErr
	}
	return a.store.UpdateMessageDelivery(ctx, reserved.ID, "sent", outboundID)
}
