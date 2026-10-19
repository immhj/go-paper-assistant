package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

const (
	maxAgentPayloadBytes = 64 * 1024
	maxJSONSafeInteger   = 9_007_199_254_740_991
)

// routerMemory is the per-turn memory rebuilt from the database for the planner (ADR 0010).
type routerMemory struct {
	CurrentProject       *projectMemory       `json:"currentProject,omitempty"`
	CurrentConversation  *conversationMemory  `json:"currentConversation,omitempty"`
	Conversations        []conversationMemory `json:"conversations"`
	Direction            string               `json:"direction,omitempty"`
	Spec                 domain.SearchSpec    `json:"spec"`
	ShownCount           int                  `json:"shownCount"`
	RecentUserMessages   []string             `json:"recentUserMessages"`
	LatestShown          []paperMemory        `json:"latestShown"`
	QuotedMessageMissing bool                 `json:"quotedMessageMissing"`
	CurrentMessage       string               `json:"currentMessage"`
}

type projectMemory struct {
	Number uint32 `json:"number"`
}

type conversationMemory struct {
	Number  uint32 `json:"number"`
	Title   string `json:"title"`
	Current bool   `json:"current"`
}

type paperMemory struct {
	Ordinal int    `json:"ordinal"`
	Title   string `json:"title"`
}

func (a *Application) buildRouterMemory(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage) (routerMemory, error) {
	memory := routerMemory{CurrentMessage: strings.TrimSpace(input.Content), Conversations: []conversationMemory{}, RecentUserMessages: []string{}, LatestShown: []paperMemory{}}
	if scope, ok := projectAgentScopeFromContext(ctx); ok {
		memory.CurrentProject = &projectMemory{Number: scope.ProjectNo}
	}
	if user.ID != 0 {
		conversations, err := a.store.ListConversations(ctx, user.ID, 100)
		if err != nil {
			return memory, err
		}
		for _, item := range conversations {
			view := conversationMemory{Number: item.ConversationNo, Title: item.Title, Current: item.ID == conversation.ID}
			memory.Conversations = append(memory.Conversations, view)
		}
	}
	if conversation.ID == 0 {
		return memory, nil
	}
	currentView := conversationMemory{Number: conversation.ConversationNo, Title: conversation.Title, Current: true}
	memory.CurrentConversation = &currentView
	current := parseSearchContext(conversation.CurrentContext, conversation.Title)
	memory.Direction, memory.Spec, memory.ShownCount = current.Name, current.Spec, len(current.Ordinals)
	recent, err := a.store.ListRecentUserMessages(ctx, conversation.ID, 11)
	if err != nil {
		return memory, err
	}
	for _, message := range recent {
		if message.Content != nil && (message.QQMessageID == nil || *message.QQMessageID != input.MessageID) {
			memory.RecentUserMessages = append(memory.RecentUserMessages, *message.Content)
		}
	}
	if len(memory.RecentUserMessages) > 10 {
		memory.RecentUserMessages = memory.RecentUserMessages[len(memory.RecentUserMessages)-10:]
	}
	var shownMessage domain.Message
	if input.ReferencedMessageID != "" {
		shownMessage, err = a.store.FindShownMessageByQQID(ctx, conversation.ID, input.ReferencedMessageID)
		if errors.Is(err, dao.ErrNotFound) {
			memory.QuotedMessageMissing = true
			return memory, nil
		}
	} else {
		shownMessage, err = a.store.FindLatestShownMessage(ctx, conversation.ID)
	}
	if errors.Is(err, dao.ErrNotFound) {
		return memory, nil
	}
	if err != nil {
		return memory, err
	}
	for _, shown := range decodeShown(shownMessage.ShownPapers) {
		paper, paperErr := a.store.GetPaper(ctx, shown.PaperID)
		if paperErr != nil {
			return memory, paperErr
		}
		memory.LatestShown = append(memory.LatestShown, paperMemory{Ordinal: shown.Ordinal, Title: paper.Title})
	}
	sort.Slice(memory.LatestShown, func(i, j int) bool { return memory.LatestShown[i].Ordinal < memory.LatestShown[j].Ordinal })
	return memory, nil
}

// toolCatalogue is every deep tool a plan step may name. Project tools are offered only when
// the turn carries a project scope (Web); the planner never sees tools it cannot use.
func toolCatalogue(ctx context.Context) []deepseek.ToolDefinition {
	tools := []deepseek.ToolDefinition{
		tool("create_conversation", "创建并切换到一个全新、无旧记忆的会话", objectSchema(map[string]any{"title": stringSchema("会话标题")}, "title")),
		tool("list_conversations", "列出当前用户所有未归档会话", emptySchema()),
		tool("switch_conversation", "按用户可见编号切换会话", objectSchema(map[string]any{"conversation_no": integerSchema(1, 1000000)}, "conversation_no")),
		tool("get_current_conversation", "读取当前会话编号和标题", emptySchema()),
		tool("rename_conversation", "重命名当前会话", objectSchema(map[string]any{"title": stringSchema("新标题")}, "title")),
		tool("archive_conversation", "归档明确指定的会话", objectSchema(map[string]any{"conversation_no": integerSchema(1, 1000000)}, "conversation_no")),
		tool("search_papers", "检索论文或继续检索未展示论文", objectSchema(map[string]any{
			"query": stringSchema("完整检索需求"), "count": integerSchema(1, maxSearchPapers),
			"mode": map[string]any{"type": "string", "enum": []string{"continue", "refine", "restart"}}, "exclude_seen": map[string]any{"type": "boolean"},
		}, "query", "count", "mode", "exclude_seen")),
		tool("list_current_papers", "列出当前会话已经展示的论文", emptySchema()),
		tool("get_paper_abstract", "查看当前会话某篇论文的完整摘要", objectSchema(map[string]any{"ordinal": integerSchema(1, 1000000)}, "ordinal")),
		tool("read_paper_quick", "快速精读当前会话某篇论文", objectSchema(map[string]any{"ordinal": integerSchema(1, 1000000)}, "ordinal")),
		tool("read_paper_deep", "完整全文深度精读当前会话某篇论文", objectSchema(map[string]any{"ordinal": integerSchema(1, 1000000)}, "ordinal")),
		tool("compare_papers", "比较当前会话的2到3篇论文", objectSchema(map[string]any{"ordinals": map[string]any{"type": "array", "items": integerSchema(1, 1000000), "minItems": 2, "maxItems": 3, "uniqueItems": true}}, "ordinals")),
		tool("answer_research_question", "基于当前会话已展示论文回答研究问题", objectSchema(map[string]any{"question": stringSchema("研究问题"), "ordinals": map[string]any{"type": "array", "items": integerSchema(1, 1000000), "minItems": 1, "maxItems": 5, "uniqueItems": true}}, "question", "ordinals")),
	}
	if _, ok := projectAgentScopeFromContext(ctx); ok {
		tools = append(tools,
			tool("list_current_project_papers", "只列出当前 active 项目的 active 论文", objectSchema(map[string]any{
				"limit": integerSchema(1, 100),
			}, "limit")),
			tool("get_current_project_paper_read_history", "读取当前 active 项目中指定论文的精读历史；历史模型输出待当前证据核对", objectSchema(map[string]any{
				"paper_id": integerSchema(1, maxJSONSafeInteger), "limit": integerSchema(1, 20),
			}, "paper_id", "limit")),
			tool("answer_from_current_project_papers", "重新读取当前 active 项目论文摘要或已校验正文后回答问题", objectSchema(map[string]any{
				"paper_ids": map[string]any{"type": "array", "items": integerSchema(1, maxJSONSafeInteger), "minItems": 1, "maxItems": 10, "uniqueItems": true}, "question": stringSchema("研究问题"),
			}, "paper_ids", "question")),
		)
	}
	return tools
}

// AgentLimits are the fixed planner limits. They are exposed so the admin surface reports
// the same numbers the executor enforces instead of restating them.
type AgentLimits struct {
	MaxSearchPapers  int `json:"maxSearchPapers"`
	ToolCallsPerTurn int `json:"toolCallsPerTurn"`
}

func Limits() AgentLimits {
	return AgentLimits{MaxSearchPapers: maxSearchPapers, ToolCallsPerTurn: maxPlanSteps}
}

func tool(name, description string, parameters map[string]any) deepseek.ToolDefinition {
	return deepseek.ToolDefinition{Type: "function", Function: deepseek.ToolFunction{Name: name, Description: description, Parameters: parameters}}
}

func emptySchema() map[string]any { return objectSchema(map[string]any{}) }
func objectSchema(properties map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func integerSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func commandToolCall(value command) deepseek.ToolCall {
	arguments := map[string]any{}
	name := ""
	switch value.Name {
	case "new":
		name, arguments["title"] = "create_conversation", value.Argument
	case "list":
		name = "list_conversations"
	case "use":
		name, arguments["conversation_no"] = "switch_conversation", mustPositiveNumber(value.Argument)
	case "current":
		name = "get_current_conversation"
	}
	encoded, _ := json.Marshal(arguments)
	return deepseek.ToolCall{Name: name, Arguments: string(encoded)}
}

func mustPositiveNumber(value string) int {
	number, _ := parsePositiveNumber(value)
	return number
}

func decodeArgs(raw string, target any) error {
	if len(raw) > maxAgentPayloadBytes {
		return errors.New("tool arguments are too large")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("tool arguments contain trailing JSON")
	}
	return nil
}

// parsedToolCall is a tool call whose arguments passed strict decoding and range checks.
// Only the fields of the named tool are set.
type parsedToolCall struct {
	Name           string
	Title          string
	ConversationNo int
	Search         searchToolPlan
	Query          string
	Ordinal        int
	Ordinals       []int
	Question       string
	Limit          int
	PaperID        uint64
	PaperIDs       []uint64
}

// parseToolCall decodes and validates a tool call. userText is the user's own words the
// deterministic search overrides read: an explicit count and an explicit "continue" win over
// whatever the model wrote (ADR 0005).
func parseToolCall(call deepseek.ToolCall, userText string, current domain.SearchContext) (parsedToolCall, error) {
	parsed := parsedToolCall{Name: call.Name}
	switch call.Name {
	case "create_conversation":
		var args struct {
			Title string `json:"title"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil {
			return parsed, err
		}
		parsed.Title = args.Title
	case "list_conversations", "get_current_conversation", "list_current_papers":
		if err := decodeArgs(call.Arguments, &struct{}{}); err != nil {
			return parsed, err
		}
	case "switch_conversation", "archive_conversation":
		var args struct {
			ConversationNo int `json:"conversation_no"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || args.ConversationNo < 1 {
			return parsed, errors.New("invalid conversation number")
		}
		parsed.ConversationNo = args.ConversationNo
	case "rename_conversation":
		var args struct {
			Title string `json:"title"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || strings.TrimSpace(args.Title) == "" {
			return parsed, errors.New("invalid conversation title")
		}
		parsed.Title = args.Title
	case "search_papers":
		var args struct {
			Query       string `json:"query"`
			Count       int    `json:"count"`
			Mode        string `json:"mode"`
			ExcludeSeen bool   `json:"exclude_seen"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil {
			return parsed, err
		}
		if strings.TrimSpace(args.Query) == "" {
			args.Query = strings.TrimSpace(userText)
		}
		if count, explicit := explicitRequestedPaperCount(userText); explicit {
			args.Count = count
		}
		if explicitContinue(userText, current) {
			args.Mode, args.ExcludeSeen = "continue", true
		}
		if args.Count < 1 || args.Count > maxSearchPapers || (args.Mode != "continue" && args.Mode != "refine" && args.Mode != "restart") {
			return parsed, errors.New("invalid paper search arguments")
		}
		parsed.Query = args.Query
		parsed.Search = searchToolPlan{Count: args.Count, Mode: args.Mode, ExcludeSeen: args.ExcludeSeen}
	case "get_paper_abstract", "read_paper_quick", "read_paper_deep":
		var args struct {
			Ordinal int `json:"ordinal"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || args.Ordinal < 1 {
			return parsed, errors.New("invalid paper ordinal")
		}
		parsed.Ordinal = args.Ordinal
	case "compare_papers":
		var args struct {
			Ordinals []int `json:"ordinals"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || !distinctOrdinals(args.Ordinals, 2, 3) {
			return parsed, errors.New("invalid comparison ordinals")
		}
		parsed.Ordinals = args.Ordinals
	case "answer_research_question":
		var args struct {
			Question string `json:"question"`
			Ordinals []int  `json:"ordinals"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || strings.TrimSpace(args.Question) == "" || !distinctOrdinals(args.Ordinals, 1, 5) {
			return parsed, errors.New("invalid research question")
		}
		parsed.Question, parsed.Ordinals = args.Question, args.Ordinals
	case "list_current_project_papers":
		var args struct {
			Limit int `json:"limit"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || args.Limit < 1 || args.Limit > 100 {
			return parsed, errors.New("invalid current project paper limit")
		}
		parsed.Limit = args.Limit
	case "get_current_project_paper_read_history":
		var args struct {
			PaperID uint64 `json:"paper_id"`
			Limit   int    `json:"limit"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || args.PaperID == 0 || args.Limit < 1 || args.Limit > 20 {
			return parsed, errors.New("invalid current project read history arguments")
		}
		parsed.PaperID, parsed.Limit = args.PaperID, args.Limit
	case "answer_from_current_project_papers":
		var args struct {
			PaperIDs []uint64 `json:"paper_ids"`
			Question string   `json:"question"`
		}
		if err := decodeArgs(call.Arguments, &args); err != nil || strings.TrimSpace(args.Question) == "" || !distinctPaperIDs(args.PaperIDs, 1, 10) {
			return parsed, errors.New("invalid current project paper answer arguments")
		}
		parsed.PaperIDs, parsed.Question = args.PaperIDs, args.Question
	default:
		return parsed, fmt.Errorf("unauthorized execution tool %q", call.Name)
	}
	return parsed, nil
}

func (a *Application) executeToolCall(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, call deepseek.ToolCall, deliver deliverer) (resultErr error) {
	if gate := toolGateFromContext(ctx); gate != nil {
		finish, err := gate.BeforeTool(ctx, call.Name)
		if err != nil {
			return err
		}
		if finish != nil {
			defer func() { resultErr = errors.Join(resultErr, finish(resultErr)) }()
		}
	}
	parsed, err := parseToolCall(call, overrideText(ctx, input.Content), parseSearchContext(conversation.CurrentContext, conversation.Title))
	if err != nil {
		return err
	}
	switch parsed.Name {
	case "create_conversation":
		return a.createConversation(ctx, user, input, parsed.Title, deliver)
	case "list_conversations":
		return a.listConversations(ctx, user, conversation, input, deliver)
	case "switch_conversation":
		return a.switchConversation(ctx, user, input, strconv.Itoa(parsed.ConversationNo), deliver)
	case "get_current_conversation":
		return deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n当前正在使用这个会话。", nil, 0)
	case "rename_conversation":
		return a.renameConversation(ctx, user, conversation, input, parsed.Title, deliver)
	case "archive_conversation":
		return a.archiveConversation(ctx, user, conversation, input, strconv.Itoa(parsed.ConversationNo), deliver)
	case "search_papers":
		return a.search(ctx, user, conversation, input, parsed.Query, parsed.Search, deliver)
	case "list_current_papers":
		return a.listCurrentPapers(ctx, conversation, input, deliver)
	case "get_paper_abstract":
		return a.getPaperAbstract(ctx, conversation, input, parsed.Ordinal, deliver)
	case "read_paper_quick":
		return a.readPaper(ctx, conversation, input, strconv.Itoa(parsed.Ordinal), domain.ProjectReadTypeQuick, deliver)
	case "read_paper_deep":
		return a.readPaper(ctx, conversation, input, strconv.Itoa(parsed.Ordinal), domain.ProjectReadTypeDeep, deliver)
	case "compare_papers":
		return a.comparePapers(ctx, conversation, input, parsed.Ordinals, deliver)
	case "answer_research_question":
		return a.answerResearchQuestion(ctx, conversation, input, parsed.Question, parsed.Ordinals, deliver)
	case "list_current_project_papers":
		return a.listCurrentProjectPapers(ctx, conversation, parsed.Limit, deliver)
	case "get_current_project_paper_read_history":
		return a.getCurrentProjectPaperReadHistory(ctx, conversation, parsed.PaperID, parsed.Limit, deliver)
	case "answer_from_current_project_papers":
		return a.answerFromCurrentProjectPapers(ctx, conversation, input, parsed.PaperIDs, parsed.Question, deliver)
	default:
		return fmt.Errorf("unauthorized execution tool %q", call.Name)
	}
}

func distinctOrdinals(values []int, minimum, maximum int) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if value < 1 {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (a *Application) recordToolUsage(messageID string, usage domain.ModelUsage) {
	if usage.CostUSD == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return
	}
	value, _ := a.toolUsage.LoadOrStore(messageID, &domain.ModelUsage{})
	value.(*domain.ModelUsage).Add(usage)
}

func (a *Application) takeToolUsage(messageID string) domain.ModelUsage {
	value, ok := a.toolUsage.LoadAndDelete(messageID)
	if !ok {
		return domain.ModelUsage{}
	}
	return *value.(*domain.ModelUsage)
}

// agentTraceVersion 3 carries the plan (ADR 0021); 2 was the Router/Executor pair.
const agentTraceVersion = 3

func agentTraceJSON(trace domain.AgentTrace) json.RawMessage {
	trace.Version = agentTraceVersion
	encoded, _ := json.Marshal(trace)
	return encoded
}

func (a *Application) persistAgentTrace(ctx context.Context, input qq.IncomingMessage, trace domain.AgentTrace, usage domain.ModelUsage) {
	if err := a.store.UpdateUserMessageAgentTrace(ctx, input.MessageID, agentTraceJSON(trace), usage.CostUSD); err != nil {
		slog.Error("persist agent trace", "message_id", input.MessageID, "error", err)
	}
}
