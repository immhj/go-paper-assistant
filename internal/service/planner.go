package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

// turnPlan is the planner's closed decision for one user message (ADR 0021): respond, clarify,
// or a validated plan of at most maxPlanSteps steps.
type turnPlan struct {
	Route         string
	Outcome       string
	Response      string
	Steps         []planStep
	Source        string
	ProviderError error
}

// plannerOutput is the exact JSON the planner model must return.
type plannerOutput struct {
	Route      string     `json:"route"`
	Outcome    string     `json:"outcome"`
	Response   string     `json:"response"`
	Steps      []planStep `json:"steps"`
	Confidence float64    `json:"confidence"`
}

const plannerSystemPrompt = `你是论文助手的规划 Agent，只决定本轮怎么做，不执行工具，不输出答案正文。只输出 JSON：{"route":"respond|clarify|plan","outcome":"用户希望达到的结果","response":"直接回复或追问文本","steps":[{"tool":"工具名","args":{},"select":{"kind":"top_relevance","count":2}}],"confidence":0.0}。
路由：问候、感谢、能力说明、明显无关输入=>respond；信息不足、论文指代不唯一或 quotedMessageMissing=true=>clarify；需要数据库、论文证据、检索、摘要、精读、比较、研究回答或会话变更=>plan。respond/clarify 必须给出 response 且 steps 为空；plan 必须给出 outcome 和 steps，response 为空。一条消息只有一个业务目标；彼此无关的多个结果（例如“重命名会话，再找论文”）要 clarify，请用户分开发送。明确“归档会话N/归档当前会话”可以 plan，模糊“这个方向没用了”要 clarify。
工具选择：出现“找、搜索、检索、更多、再来”用 search_papers；“列出、有哪些、当前论文”用 list_current_papers；“完整摘要”用 get_paper_abstract；“快速精读”用 read_paper_quick；只有用户明确说“深度精读”才用 read_paper_deep；只有用户明确说“对比/比较”才用 compare_papers；当前会话研究问题用 answer_research_question；当前项目论文列表用 list_current_project_papers；用户说的“论文 ID N”就是 paper_id=N；指定当前项目 paper_id 的精读历史用 get_current_project_paper_read_history；基于本轮明确指定的当前项目 paper_ids 回答用 answer_from_current_project_papers；会话动作按对应工具。
计划：steps 按执行顺序排列，最多 maxSteps 步，能一步完成就只写一步。search_papers 最多一次且只能是第一步；其后只能是 get_paper_abstract、read_paper_quick、read_paper_deep、compare_papers、answer_research_question；其他工具只能单独一步。深度精读、对比、项目论文问答合计最多一次。后续步骤要处理第一步检索出来的论文时，args 里不写 ordinal/ordinals，改写 select：{"kind":"top_relevance","count":k} 表示检索结果中最相关的前 k 篇；{"kind":"position","positions":[n]} 表示检索结果中的第 n 篇。处理当前会话已有序号的论文时直接写 ordinal/ordinals，不写 select。超出这些规则的需求用 clarify 请用户拆开发送。`

// planTurn decides a turn: slash commands and fallbacks are rules, everything else is one
// planner call whose plan is validated against the transition table before anything runs.
// maxSteps is maxPlanSteps on the Web and 1 on QQ.
func (a *Application) planTurn(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, parsed command, maxSteps int) (turnPlan, domain.ModelUsage) {
	if parsed.Name == "help" {
		return turnPlan{Route: "respond", Response: helpText(), Source: "rule"}, domain.ModelUsage{}
	}
	if parsed.Name != "" {
		if parsed.Name == "use" {
			if _, err := parsePositiveNumber(parsed.Argument); err != nil {
				return turnPlan{Route: "clarify", Response: "格式：/use 会话编号，例如 /use 2", Source: "rule"}, domain.ModelUsage{}
			}
		}
		outcome := map[string]string{
			"new": "创建并切换到一个全新会话", "list": "列出可用会话", "use": "切换当前会话", "current": "查看当前会话",
		}[parsed.Name]
		call := commandToolCall(parsed)
		return turnPlan{Route: "plan", Outcome: outcome, Source: "rule", Steps: []planStep{{Tool: call.Name, Args: json.RawMessage(call.Arguments)}}}, domain.ModelUsage{}
	}
	text := strings.TrimSpace(input.Content)
	if strings.HasPrefix(text, "/") {
		return turnPlan{Route: "respond", Response: "这个业务命令已移除。请直接用自然语言描述，例如“快速精读第三篇”或“归档会话2”。", Source: "rule"}, domain.ModelUsage{}
	}
	if !a.model.Available() {
		return turnPlan{Route: "respond", Response: "论文助手暂时无法理解自然语言请求。你仍可使用 /new、/list、/use、/current 和 /help。", Source: "fallback"}, domain.ModelUsage{}
	}
	memory, err := a.buildRouterMemory(ctx, user, conversation, input)
	if err != nil {
		slog.Error("build planner memory", "error", err)
		return turnPlan{Route: "respond", Response: "暂时无法读取当前会话记忆，请稍后重试。", Source: "fallback"}, domain.ModelUsage{}
	}
	type toolView struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	tools := make([]toolView, 0, 16)
	for _, definition := range toolCatalogue(ctx) {
		tools = append(tools, toolView{Name: definition.Function.Name, Description: definition.Function.Description, Parameters: definition.Function.Parameters})
	}
	inputJSON, _ := json.Marshal(map[string]any{"memory": memory, "tools": tools, "maxSteps": maxSteps})
	content, usage, err := a.model.Complete(ctx, deepseek.Request{JSON: true, Messages: []deepseek.Message{
		{Role: "system", Content: strings.ReplaceAll(plannerSystemPrompt, "maxSteps", fmt.Sprint(maxSteps))},
		{Role: "user", Content: string(inputJSON)},
	}})
	if err != nil {
		slog.Error("planner agent failed", "error", err)
		return turnPlan{Route: "respond", Response: "论文助手暂时无法处理这条消息，请稍后重试。", Source: "fallback", ProviderError: err}, usage
	}
	plan, err := decodePlannerOutput(content)
	if err != nil {
		slog.Error("planner decision invalid", "error", err)
		return turnPlan{Route: "clarify", Response: "我没有准确理解你的目标，请换一种更具体的说法。", Source: "fallback"}, usage
	}
	plan.Source = "llm"
	if plan.Route != "plan" {
		return plan, usage
	}
	current := parseSearchContext(conversation.CurrentContext, conversation.Title)
	if err = validatePlan(plan.Steps, input.Content, current, maxSteps); err != nil {
		var rejection *planRejection
		if !errors.As(err, &rejection) {
			rejection = &planRejection{reason: "我没有准确理解你的目标，请换一种更具体的说法。"}
		}
		slog.Warn("planner plan rejected", "steps", planToolNames(plan.Steps), "reason", rejection.reason)
		return turnPlan{Route: "clarify", Outcome: plan.Outcome, Response: rejection.reason, Steps: plan.Steps, Source: "llm"}, usage
	}
	return plan, usage
}

func planToolNames(steps []planStep) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Tool)
	}
	return names
}

func decodePlannerOutput(content string) (turnPlan, error) {
	content = stripCodeFence(content)
	if len(content) > maxAgentPayloadBytes {
		return turnPlan{}, errors.New("planner decision is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var output plannerOutput
	if err := decoder.Decode(&output); err != nil {
		return turnPlan{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return turnPlan{}, errors.New("planner decision has trailing JSON")
	}
	if output.Confidence < 0 || output.Confidence > 1 {
		return turnPlan{}, errors.New("planner confidence is out of range")
	}
	switch output.Route {
	case "respond", "clarify":
		if strings.TrimSpace(output.Response) == "" || len([]rune(output.Response)) > 800 {
			return turnPlan{}, errors.New("planner response is missing or too long")
		}
		return turnPlan{Route: output.Route, Response: output.Response}, nil
	case "plan":
		if strings.TrimSpace(output.Outcome) == "" || len([]rune(output.Outcome)) > 500 || len(output.Steps) == 0 {
			return turnPlan{}, errors.New("planner plan is invalid")
		}
		for index := range output.Steps {
			if len(output.Steps[index].Args) == 0 || string(output.Steps[index].Args) == "null" {
				output.Steps[index].Args = json.RawMessage(`{}`)
			}
		}
		return turnPlan{Route: "plan", Outcome: output.Outcome, Steps: output.Steps}, nil
	default:
		return turnPlan{}, fmt.Errorf("unknown planner route %q", output.Route)
	}
}
