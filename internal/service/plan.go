package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"paperassistant/internal/domain"
	"paperassistant/internal/provider/deepseek"
)

// maxPlanSteps bounds a Web plan (ADR 0021). QQ plans are always one step.
const maxPlanSteps = 3

// planStep is one tool call in a plan. Args never carry the papers a selector picks: those are
// resolved in Go after the first (search) step has been delivered.
type planStep struct {
	Tool   string               `json:"tool"`
	Args   json.RawMessage      `json:"args"`
	Select *domain.PlanSelector `json:"select,omitempty"`
}

// planRejection is a plan the system will not run. Its message is user-facing: the turn ends
// with a clarification asking the user to split the request (ADR 0021 decision 6).
type planRejection struct{ reason string }

func (r *planRejection) Error() string { return r.reason }

func reject(format string, values ...any) error {
	return &planRejection{reason: fmt.Sprintf(format, values...)}
}

// planTools classifies every tool a plan may name.
var (
	// paperStepTools may follow a search; they consume papers and nothing else.
	paperStepTools = map[string]bool{
		"get_paper_abstract": true, "read_paper_quick": true, "read_paper_deep": true,
		"compare_papers": true, "answer_research_question": true,
	}
	// singleStepTools only run as a one-step plan.
	singleStepTools = map[string]bool{
		"create_conversation": true, "list_conversations": true, "switch_conversation": true,
		"get_current_conversation": true, "rename_conversation": true, "archive_conversation": true,
		"list_current_papers": true, "list_current_project_papers": true,
		"get_current_project_paper_read_history": true, "answer_from_current_project_papers": true,
	}
	conversationManagementTools = map[string]bool{
		"create_conversation": true, "list_conversations": true, "switch_conversation": true,
		"get_current_conversation": true, "rename_conversation": true, "archive_conversation": true,
	}
	// thinkingTools read full text or many papers with thinking on; a plan holds at most one.
	thinkingTools = map[string]bool{"read_paper_deep": true, "compare_papers": true, "answer_from_current_project_papers": true}
	// selectionSize is how many papers a selector must pick for each paper tool.
	selectionSize = map[string][2]int{
		"get_paper_abstract": {1, 1}, "read_paper_quick": {1, 1}, "read_paper_deep": {1, 1},
		"compare_papers": {2, 3}, "answer_research_question": {1, 5},
	}
)

var toolLabels = map[string]string{
	"search_papers": "论文检索", "get_paper_abstract": "查看摘要", "read_paper_quick": "快速精读",
	"read_paper_deep": "深度精读", "compare_papers": "论文对比", "answer_research_question": "研究问答",
}

func toolLabel(name string) string {
	if label, ok := toolLabels[name]; ok {
		return label
	}
	return name
}

// heavyToolKeywords are the user's own words that authorize a high-cost step (ADR 0021
// decision 5). The model cannot add a deep read or a comparison the user did not ask for.
var heavyToolKeywords = map[string][]string{
	"read_paper_deep": {"深度精读", "深读", "深度阅读", "deep read"},
	// Bare "比较" is left out: in "比较新的论文" it means "fairly", not "compare".
	"compare_papers": {"对比", "compare", "区别", "异同", "比较一下", "比较第", "比较前", "比较这", "比较它们", "进行比较", "做比较", "做个比较"},
}

// heavyToolNegations extend the source negations with forms that commonly precede a
// declined analysis, such as "不需要对比".
var heavyToolNegations = []string{"不需要", "无需", "不必", "别", "不"}

// userRequests reports whether the user's text affirmatively names one of the keywords; a
// negated mention such as "不用对比" does not count.
func userRequests(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, keyword := range keywords {
		for offset := 0; ; {
			index := strings.Index(lower[offset:], keyword)
			if index < 0 {
				break
			}
			start := offset + index
			if !negatedBefore(lower[:start]) {
				return true
			}
			offset = start + len(keyword)
		}
	}
	return false
}

// negatedBefore reports whether the text right before a keyword is a negation phrase.
func negatedBefore(before string) bool {
	before = strings.TrimRight(before, " ")
	for _, negation := range append(append([]string(nil), sourceNegations...), heavyToolNegations...) {
		if strings.HasSuffix(before, strings.TrimSpace(negation)) {
			return true
		}
	}
	return false
}

// searchStepBoundaries end the part of a multi-step message that describes the search, so a
// count like "两篇" in "然后对比最相关的两篇" is not read as the search count.
var searchStepBoundaries = []string{"，", ",", "。", "；", ";", "然后", "之后", "接着", "随后", "并且"}

// searchSegment returns the leading part of a message up to its first step boundary.
func searchSegment(text string) string {
	end := len(text)
	for _, boundary := range searchStepBoundaries {
		if index := strings.Index(text, boundary); index >= 0 && index < end {
			end = index
		}
	}
	return strings.TrimSpace(text[:end])
}

// stepTarget identifies the papers a paper step reads before execution: a literal conversation
// ordinal ("o3") or a position in the first step's displayed list ("p1").
func stepTargets(step planStep) ([]string, error) {
	if step.Select != nil {
		positions, err := selectorPositions(*step.Select)
		if err != nil {
			return nil, err
		}
		targets := make([]string, 0, len(positions))
		for _, position := range positions {
			targets = append(targets, fmt.Sprintf("p%d", position))
		}
		return targets, nil
	}
	var args struct {
		Ordinal  int   `json:"ordinal"`
		Ordinals []int `json:"ordinals"`
	}
	_ = json.Unmarshal(step.Args, &args)
	targets := []string{}
	if args.Ordinal > 0 {
		targets = append(targets, fmt.Sprintf("o%d", args.Ordinal))
	}
	for _, ordinal := range args.Ordinals {
		targets = append(targets, fmt.Sprintf("o%d", ordinal))
	}
	return targets, nil
}

// selectorPositions expands a selector into 1-based positions of the displayed list.
func selectorPositions(selector domain.PlanSelector) ([]int, error) {
	switch selector.Kind {
	case "top_relevance":
		if selector.Count < 1 || selector.Count > maxSearchPapers || len(selector.Positions) > 0 {
			return nil, reject("引用检索结果的篇数无效")
		}
		positions := make([]int, selector.Count)
		for index := range positions {
			positions[index] = index + 1
		}
		return positions, nil
	case "position":
		if selector.Count != 0 || !distinctOrdinals(selector.Positions, 1, maxSearchPapers) {
			return nil, reject("引用检索结果的位置无效")
		}
		for _, position := range selector.Positions {
			if position > maxSearchPapers {
				return nil, reject("引用检索结果的位置无效")
			}
		}
		return append([]int(nil), selector.Positions...), nil
	default:
		return nil, reject("引用检索结果的方式不受支持")
	}
}

// withSelectedOrdinals returns args with the ordinal(s) a paper tool takes set to ordinals.
func withSelectedOrdinals(tool string, args json.RawMessage, ordinals []int) (json.RawMessage, error) {
	fields := map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &fields); err != nil {
			return nil, err
		}
	}
	if tool == "compare_papers" || tool == "answer_research_question" {
		fields["ordinals"] = ordinals
	} else {
		fields["ordinal"] = ordinals[0]
	}
	return json.Marshal(fields)
}

// validatePlan enforces the transition table of ADR 0021 before anything runs, so a plan is
// either executed as a whole bounded sequence or rejected with a request to split it.
// maxSteps is maxPlanSteps on the Web and 1 on QQ.
func validatePlan(steps []planStep, userText string, current domain.SearchContext, maxSteps int) error {
	if len(steps) == 0 {
		return reject("没有可以执行的步骤")
	}
	if len(steps) > maxSteps {
		if maxSteps == 1 {
			return reject("QQ 暂时只支持一条消息完成一个步骤，请分开发送，例如先检索，再说“对比第1篇和第2篇”。")
		}
		return reject("一条消息最多可以完成 %d 个步骤，请把需求拆成几条消息分别发送。", maxSteps)
	}
	thinking := 0
	for index, step := range steps {
		if step.Tool != "search_papers" && !paperStepTools[step.Tool] && !singleStepTools[step.Tool] {
			return reject("这个请求包含不支持的操作，请换一种说法。")
		}
		if len(steps) > 1 && singleStepTools[step.Tool] {
			return reject("会话、项目管理或列表类操作需要单独发送，请把它和其他需求分开发送。")
		}
		if step.Tool == "search_papers" && index > 0 {
			return reject("论文检索只能作为第一个步骤，且一条消息只检索一次，请把需求分开发送。")
		}
		if thinkingTools[step.Tool] {
			thinking++
		}
		if keywords, ok := heavyToolKeywords[step.Tool]; ok && !userRequests(userText, keywords) {
			if step.Tool == "read_paper_deep" {
				return reject("需要深度精读时，请明确说“深度精读第N篇”。")
			}
			return reject("需要对比论文时，请明确说“对比第N篇和第M篇”。")
		}
	}
	if thinking > 1 {
		return reject("一条消息最多包含一次深度精读、对比或项目论文问答这类高成本步骤，请分开发送。")
	}
	if err := validateSelections(steps); err != nil {
		return err
	}
	if len(steps) == 1 {
		// One-step plans keep the existing argument handling, which reports invalid arguments
		// as a failed task rather than a clarification.
		return nil
	}
	for index, step := range steps {
		args := step.Args
		if step.Select != nil {
			positions, _ := selectorPositions(*step.Select)
			var err error
			if args, err = withSelectedOrdinals(step.Tool, args, positions); err != nil {
				return reject("步骤 %d 的参数无效，请换一种说法。", index+1)
			}
		}
		text := userText
		if step.Tool == "search_papers" {
			text = searchSegment(userText)
		}
		if _, err := parseToolCall(deepseek.ToolCall{Name: step.Tool, Arguments: string(args)}, text, current); err != nil {
			return reject("步骤 %d（%s）的参数无效，请换一种说法。", index+1, toolLabel(step.Tool))
		}
	}
	return nil
}

// validateSelections checks selectors and the rule that one plan never quick-reads and
// deep-reads the same paper (CONTEXT.md 计划).
func validateSelections(steps []planStep) error {
	var quick, deep []string
	for index, step := range steps {
		if step.Select != nil {
			if index == 0 || steps[0].Tool != "search_papers" {
				return reject("只有先检索，后面的步骤才能引用检索结果，请明确论文序号。")
			}
			bounds, ok := selectionSize[step.Tool]
			if !ok {
				return reject("步骤 %d 不能引用检索结果。", index+1)
			}
			if hasOrdinalArgs(step.Args) {
				return reject("步骤 %d 指定的论文不明确，请明确论文序号。", index+1)
			}
			positions, err := selectorPositions(*step.Select)
			if err != nil {
				return err
			}
			if len(positions) < bounds[0] || len(positions) > bounds[1] {
				return reject("%s需要 %d 到 %d 篇论文，请明确要处理哪几篇。", toolLabel(step.Tool), bounds[0], bounds[1])
			}
		}
		targets, err := stepTargets(step)
		if err != nil {
			return err
		}
		switch step.Tool {
		case "read_paper_quick":
			quick = append(quick, targets...)
		case "read_paper_deep":
			deep = append(deep, targets...)
		}
	}
	if len(quick) > 0 && len(deep) > 0 && !disjointTargets(quick, deep) {
		return reject("同一篇论文的快速精读和深度精读需要分开发送：先快速精读，再决定是否深度精读。")
	}
	return nil
}

func hasOrdinalArgs(args json.RawMessage) bool {
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(args, &fields)
	_, ordinal := fields["ordinal"]
	_, ordinals := fields["ordinals"]
	return ordinal || ordinals
}

// disjointTargets is true only when the two target lists provably name different papers. A
// literal ordinal and a list position cannot be compared before execution, so they conflict.
func disjointTargets(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			if a == b || a[0] != b[0] {
				return false
			}
		}
	}
	return true
}
