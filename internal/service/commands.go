package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
)

type command struct {
	Name     string
	Argument string
}

func parseCommand(content string) command {
	text := strings.TrimSpace(content)
	if text == "" {
		return command{Name: "help"}
	}
	parts := strings.SplitN(text, " ", 2)
	name := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	argument := ""
	if len(parts) == 2 {
		argument = strings.TrimSpace(parts[1])
	}
	aliases := map[string]string{
		"new": "new", "新建": "new", "list": "list", "列表": "list",
		"use": "use", "切换": "use", "current": "current", "当前": "current",
		"help": "help", "帮助": "help",
	}
	if resolved, ok := aliases[name]; ok && strings.HasPrefix(text, "/") {
		return command{Name: resolved, Argument: argument}
	}
	return command{}
}

func (a *Application) createConversation(ctx context.Context, user domain.User, input qq.IncomingMessage, title string, deliver deliverer) error {
	if strings.TrimSpace(title) == "" {
		title = "新会话"
	}
	current, _ := json.Marshal(newSearchContext(title))
	exists, err := a.store.HasUserMessage(ctx, input.MessageID)
	if err != nil {
		return err
	}
	var conversation domain.Conversation
	if exists {
		conversation, err = a.store.CreateConversation(ctx, user.ID, title, current)
	} else {
		conversation, err = a.store.CreateConversationForMessage(ctx, user.ID, title, current, input.MessageID, input.Content)
	}
	if err != nil {
		return err
	}
	if exists {
		if err = a.recordUserMessage(ctx, conversation.ID, input); err != nil {
			return err
		}
	}
	return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n已新建并切换。直接描述你想找的论文即可。", nil, 0)
}

func (a *Application) listConversations(ctx context.Context, user domain.User, current domain.Conversation, input qq.IncomingMessage, deliver deliverer) error {
	items, err := a.store.ListConversations(ctx, user.ID, 10)
	if err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("最近会话：\n")
	for _, item := range items {
		marker := "  "
		if item.ID == current.ID {
			marker = "→ "
		}
		fmt.Fprintf(&builder, "%s#%d %s\n", marker, item.ConversationNo, item.Title)
	}
	builder.WriteString("\n使用 /use 编号 切换。")
	return deliver.Text(ctx, current, builder.String(), nil, 0)
}

func (a *Application) switchConversation(ctx context.Context, user domain.User, input qq.IncomingMessage, argument string, deliver deliverer) error {
	number, err := parsePositiveNumber(argument)
	if err != nil {
		active, _ := a.ensureActiveConversation(ctx, user)
		return deliver.Text(ctx, active, "格式：/use 会话编号，例如 /use 2", nil, 0)
	}
	conversation, err := a.store.SwitchConversation(ctx, user.ID, uint32(number))
	if errors.Is(err, dao.ErrNotFound) {
		active, _ := a.ensureActiveConversation(ctx, user)
		return deliver.Text(ctx, active, fmt.Sprintf("找不到会话 #%d。使用 /list 查看。", number), nil, 0)
	}
	if err != nil {
		return err
	}
	return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n已切换到这个会话。", nil, 0)
}

func (a *Application) renameConversation(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, title string, deliver deliverer) error {
	if strings.TrimSpace(title) == "" {
		return deliver.Text(ctx, conversation, "格式：/rename 新名称", nil, 0)
	}
	if err := a.store.RenameConversation(ctx, user.ID, conversation.ID, title); err != nil {
		return err
	}
	conversation.Title = strings.TrimSpace(title)
	return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n已重命名。", nil, 0)
}

func (a *Application) archiveConversation(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, argument string, deliver deliverer) error {
	number, err := parsePositiveNumber(argument)
	if err != nil {
		return deliver.Text(ctx, conversation, "格式：/delete 会话编号，例如 /delete 2", nil, 0)
	}
	if err = a.store.ArchiveConversation(ctx, user.ID, uint32(number)); errors.Is(err, dao.ErrNotFound) {
		return deliver.Text(ctx, conversation, fmt.Sprintf("找不到会话 #%d。", number), nil, 0)
	}
	if err != nil {
		return err
	}
	active, activeErr := a.store.GetActiveConversation(ctx, user.ID)
	if activeErr == nil {
		return deliver.Text(ctx, active, fmt.Sprintf("已归档会话 #%d。\n%s", number, conversationHeader(active)), nil, 0)
	}
	fresh, createErr := a.store.CreateConversation(ctx, user.ID, "默认会话", nil)
	if createErr != nil {
		return createErr
	}
	return deliver.Text(ctx, fresh, fmt.Sprintf("已归档会话 #%d，并创建新的默认会话。", number), nil, 0)
}

func parsePositiveNumber(value string) (int, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "#"))
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return 0, errors.New("expected a positive number")
	}
	return number, nil
}

func conversationHeader(item domain.Conversation) string {
	return fmt.Sprintf("[会话 #%d · %s]", item.ConversationNo, item.Title)
}

func helpText() string {
	return `论文助手会话命令：
/new 标题    新建并切换到干净会话
/list         查看会话
/use 2        切换会话
/current      当前会话
/help         帮助

论文业务请直接用自然语言，例如：
“找10篇DeepSeek量化论文”
“再找5篇”
“查看第2篇完整摘要”
“快速精读第3篇”
“深度精读第3篇”
“比较第2篇和第4篇”
“把当前会话改名为量化”
“归档会话2”`
}
