package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"paperassistant/internal/domain"
	"paperassistant/internal/provider/deepseek"
)

const maxWebConversationTitleRunes = 24

func (a *Application) autoTitleWebConversation(ctx context.Context, actor WebActor, projectNo uint32, conversation domain.Conversation, firstMessage string) (domain.Conversation, domain.ModelUsage) {
	if a.model == nil || !isPlaceholderWebConversationTitle(conversation.Title) {
		return conversation, domain.ModelUsage{}
	}

	title := fallbackWebConversationTitle(firstMessage)
	var usage domain.ModelUsage
	if a.model.Available() {
		content, modelUsage, err := a.model.Complete(ctx, deepseek.Request{JSON: true, Messages: []deepseek.Message{
			{Role: "system", Content: `你负责给论文助手的对话生成简短标题。根据用户首条消息概括核心任务或研究主题，只输出 JSON：{"title":"标题"}。标题使用与用户相同的语言，中文通常 4 到 18 个字，不加引号外的解释，不使用“新会话”“默认会话”等泛化名称。`},
			{Role: "user", Content: truncateWebTitleSource(firstMessage)},
		}})
		usage = modelUsage
		if err != nil {
			slog.Warn("generate web conversation title", "conversation_id", conversation.ID, "error", err)
		} else if generated, decodeErr := decodeWebConversationTitle(content); decodeErr != nil {
			slog.Warn("decode web conversation title", "conversation_id", conversation.ID, "error", decodeErr)
		} else {
			title = generated
		}
	}
	if title == "" || isPlaceholderWebConversationTitle(title) {
		title = "新话题"
	}

	owner, ownerErr := projectOwner(actor)
	var err error
	if ownerErr != nil {
		err = ownerErr
	} else {
		_, err = a.store.RenameProjectConversation(ctx, owner, projectNo, conversation.ConversationNo, title)
	}
	if err != nil {
		// Naming is cosmetic and must never make an otherwise valid chat turn fail.
		slog.Warn("persist web conversation title", "conversation_id", conversation.ID, "actor_type", actor.Type, "error", err)
		return conversation, usage
	}
	conversation.Title = title
	return conversation, usage
}

func isPlaceholderWebConversationTitle(title string) bool {
	switch strings.TrimSpace(title) {
	case "新会话", "默认会话":
		return true
	default:
		return false
	}
}

func decodeWebConversationTitle(content string) (string, error) {
	var payload struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", err
	}
	title := normalizeWebConversationTitle(payload.Title)
	if title == "" {
		return "", errors.New("empty generated title")
	}
	return title, nil
}

func fallbackWebConversationTitle(firstMessage string) string {
	title := strings.TrimLeft(normalizeWebConversationTitle(firstMessage), "/")
	if title == "" {
		return "新话题"
	}
	return title
}

func normalizeWebConversationTitle(value string) string {
	title := strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	title = strings.Trim(title, "\\\"'“”‘’")
	title = strings.TrimRight(title, "。.!！?？")
	runes := []rune(title)
	if len(runes) > maxWebConversationTitleRunes {
		title = string(runes[:maxWebConversationTitleRunes])
	}
	return strings.TrimSpace(title)
}

func truncateWebTitleSource(value string) string {
	const maxRunes = 500
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return string(runes)
}
