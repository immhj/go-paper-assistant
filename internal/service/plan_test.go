package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

func searchStep(count int) planStep {
	args, _ := json.Marshal(map[string]any{"query": "联邦学习", "count": count, "mode": "restart", "exclude_seen": false})
	return planStep{Tool: "search_papers", Args: args}
}

func selectStep(tool string, selector domain.PlanSelector, extra map[string]any) planStep {
	args, _ := json.Marshal(extra)
	if extra == nil {
		args = json.RawMessage(`{}`)
	}
	return planStep{Tool: tool, Args: args, Select: &selector}
}

func literalStep(tool string, args string) planStep {
	return planStep{Tool: tool, Args: json.RawMessage(args)}
}

func top(count int) domain.PlanSelector {
	return domain.PlanSelector{Kind: "top_relevance", Count: count}
}

func TestValidatePlanAcceptsBoundedPaperPlans(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		steps []planStep
	}{
		{"single search", "找 5 篇联邦学习论文", []planStep{searchStep(5)}},
		{"search then compare the two most relevant", "找 5 篇 IEEE 联邦学习论文，对比最相关的两篇",
			[]planStep{searchStep(5), selectStep("compare_papers", top(2), nil)}},
		{"search then quick read the first", "找 3 篇扩散模型分割的论文，快速精读第一篇",
			[]planStep{searchStep(3), selectStep("read_paper_quick", domain.PlanSelector{Kind: "position", Positions: []int{1}}, nil)}},
		{"search then deep read the most relevant", "找论文然后深度精读最相关的",
			[]planStep{searchStep(5), selectStep("read_paper_deep", top(1), nil)}},
		{"search, quick read, then research question", "找 5 篇论文，快速精读第一篇，再回答它们解决了什么问题",
			[]planStep{searchStep(5), selectStep("read_paper_quick", top(1), nil),
				selectStep("answer_research_question", top(3), map[string]any{"question": "解决了什么问题"})}},
		{"literal ordinals without search", "快速精读第 3 篇，然后对比第 3 篇和第 5 篇",
			[]planStep{literalStep("read_paper_quick", `{"ordinal":3}`), literalStep("compare_papers", `{"ordinals":[3,5]}`)}},
	}
	for _, tc := range cases {
		if err := validatePlan(tc.steps, tc.text, domain.SearchContext{}, maxPlanSteps); err != nil {
			t.Errorf("%s: unexpected rejection: %v", tc.name, err)
		}
	}
}

func TestValidatePlanRejectsPlansOutsideTheTransitionTable(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		steps []planStep
		want  string // part of the user-facing reason
	}{
		{"too many steps", "找论文，快速精读第一篇，再查看第二篇摘要，再回答问题",
			[]planStep{searchStep(5), selectStep("read_paper_quick", top(1), nil), selectStep("get_paper_abstract", domain.PlanSelector{Kind: "position", Positions: []int{2}}, nil), selectStep("answer_research_question", top(2), map[string]any{"question": "x"})}, "3"},
		{"two thinking steps", "找 5 篇然后深度精读最相关的，再对比前两篇",
			[]planStep{searchStep(5), selectStep("read_paper_deep", top(1), nil), selectStep("compare_papers", top(2), nil)}, "高成本"},
		{"search not first", "快速精读第 3 篇，再找 5 篇论文",
			[]planStep{literalStep("read_paper_quick", `{"ordinal":3}`), searchStep(5)}, "检索"},
		{"two searches", "找论文再找论文",
			[]planStep{searchStep(5), searchStep(5)}, "检索"},
		{"conversation management combined", "重命名会话为 X，再找 5 篇论文",
			[]planStep{literalStep("rename_conversation", `{"title":"X"}`), searchStep(5)}, "分开"},
		{"project tool combined", "找论文再列出项目论文",
			[]planStep{searchStep(5), literalStep("list_current_project_papers", `{"limit":10}`)}, "分开"},
		{"quick and deep of the same paper", "快速精读第 3 篇然后深度精读",
			[]planStep{literalStep("read_paper_quick", `{"ordinal":3}`), literalStep("read_paper_deep", `{"ordinal":3}`)}, "快速精读和深度精读"},
		{"deep read the model added itself", "找论文然后仔细读一下最相关的",
			[]planStep{searchStep(5), selectStep("read_paper_deep", top(1), nil)}, "深度精读"},
		{"comparison the user negated", "找 5 篇论文，不用对比，快速精读第一篇",
			[]planStep{searchStep(5), selectStep("compare_papers", top(2), nil)}, "对比"},
		{"selector without a search first", "快速精读第 3 篇然后对比最相关的两篇",
			[]planStep{literalStep("read_paper_quick", `{"ordinal":3}`), selectStep("compare_papers", top(2), nil)}, "检索结果"},
		{"selector size wrong for compare", "找论文然后对比最相关的",
			[]planStep{searchStep(5), selectStep("compare_papers", top(1), nil)}, "对比"},
		{"selector and literal ordinals together", "找论文然后快速精读第一篇",
			[]planStep{searchStep(5), selectStep("read_paper_quick", top(1), map[string]any{"ordinal": 1})}, "论文"},
		{"unknown tool", "做点什么", []planStep{literalStep("delete_everything", `{}`)}, "不支持"},
		{"empty plan", "找论文", nil, "步骤"},
	}
	for _, tc := range cases {
		err := validatePlan(tc.steps, tc.text, domain.SearchContext{}, maxPlanSteps)
		var rejection *planRejection
		if !errors.As(err, &rejection) {
			t.Errorf("%s: want a plan rejection, got %v", tc.name, err)
			continue
		}
		if !strings.Contains(rejection.Error(), tc.want) {
			t.Errorf("%s: rejection %q does not mention %q", tc.name, rejection.Error(), tc.want)
		}
	}
}

func TestValidatePlanOnQQAllowsOnlyOneStep(t *testing.T) {
	steps := []planStep{searchStep(5), selectStep("compare_papers", top(2), nil)}
	err := validatePlan(steps, "找 5 篇然后对比最相关两篇", domain.SearchContext{}, 1)
	var rejection *planRejection
	if !errors.As(err, &rejection) || !strings.Contains(rejection.Error(), "分开发送") {
		t.Fatalf("QQ multi-step plan must ask to split, got %v", err)
	}
	if err = validatePlan([]planStep{searchStep(5)}, "找 5 篇论文", domain.SearchContext{}, 1); err != nil {
		t.Fatalf("QQ single step must pass: %v", err)
	}
}

func TestValidatePlanAppliesTheUsersLiteralSearchCount(t *testing.T) {
	// The model asked for 20, but the user's own words say 5 before the first step boundary.
	steps := []planStep{searchStep(20), selectStep("compare_papers", top(2), nil)}
	if err := validatePlan(steps, "找 5 篇联邦学习论文，对比最相关的两篇", domain.SearchContext{}, maxPlanSteps); err != nil {
		t.Fatalf("explicit count in the search segment should override the model: %v", err)
	}
	if got := searchSegment("找论文然后对比两篇"); got != "找论文" {
		t.Fatalf("search segment = %q", got)
	}
	if got := searchSegment("找 5 篇联邦学习论文，对比最相关的两篇"); got != "找 5 篇联邦学习论文" {
		t.Fatalf("search segment = %q", got)
	}
}

// plannerTestModel returns a fixed planner decision and records how often it was asked.
type plannerTestModel struct {
	content string
	calls   int
	request deepseek.Request
}

func (m *plannerTestModel) Available() bool { return true }
func (m *plannerTestModel) Complete(_ context.Context, request deepseek.Request) (string, domain.ModelUsage, error) {
	m.calls++
	m.request = request
	return m.content, domain.ModelUsage{CostUSD: 0.01}, nil
}
func (m *plannerTestModel) CallTool(context.Context, deepseek.ToolRequest) (deepseek.ToolCall, domain.ModelUsage, error) {
	return deepseek.ToolCall{}, domain.ModelUsage{}, errors.New("the planner never uses function calling")
}
func (m *plannerTestModel) Stream(context.Context, deepseek.Request, func(string) error) (domain.ModelUsage, error) {
	return domain.ModelUsage{}, errors.New("not used")
}

const searchThenCompare = `{"route":"plan","outcome":"检索并对比","response":"","confidence":0.9,"steps":[
 {"tool":"search_papers","args":{"query":"IEEE 联邦学习","count":5,"mode":"restart","exclude_seen":false}},
 {"tool":"compare_papers","args":{},"select":{"kind":"top_relevance","count":2}}]}`

func TestPlanTurnReturnsAValidatedWebPlanFromOnePlannerCall(t *testing.T) {
	model := &plannerTestModel{content: searchThenCompare}
	app := &Application{model: model}
	input := qq.IncomingMessage{Content: "找 5 篇 IEEE 联邦学习论文，对比最相关的两篇"}
	plan, usage := app.planTurn(context.Background(), domain.User{}, domain.Conversation{}, input, command{}, maxPlanSteps)
	if plan.Route != "plan" || len(plan.Steps) != 2 || plan.Steps[1].Select == nil || plan.Steps[1].Select.Count != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if model.calls != 1 || usage.CostUSD != 0.01 {
		t.Fatalf("planning must be exactly one model call, got %d (usage %+v)", model.calls, usage)
	}
	if !strings.Contains(model.request.Messages[1].Content, `"maxSteps":3`) {
		t.Fatal("planner input must state the step budget")
	}
}

func TestPlanTurnAsksToSplitWhenThePlanBreaksTheRules(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		content  string
		maxSteps int
		want     string
	}{
		{"multi-step on QQ", "找 5 篇然后对比最相关两篇", searchThenCompare, 1, "分开发送"},
		{"deep read the user did not ask for", "找论文然后仔细看看最相关的", `{"route":"plan","outcome":"x","response":"","confidence":0.8,"steps":[
			{"tool":"search_papers","args":{"query":"x","count":5,"mode":"restart","exclude_seen":false}},
			{"tool":"read_paper_deep","args":{},"select":{"kind":"top_relevance","count":1}}]}`, maxPlanSteps, "深度精读"},
		{"unusable planner JSON", "找论文", `{"route":"plan","steps":"oops"}`, maxPlanSteps, "换一种更具体的说法"},
	}
	for _, tc := range cases {
		app := &Application{model: &plannerTestModel{content: tc.content}}
		plan, _ := app.planTurn(context.Background(), domain.User{}, domain.Conversation{}, qq.IncomingMessage{Content: tc.text}, command{}, tc.maxSteps)
		if plan.Route != "clarify" || !strings.Contains(plan.Response, tc.want) {
			t.Errorf("%s: want a clarification mentioning %q, got %+v", tc.name, tc.want, plan)
		}
	}
}

func TestPlanTurnTurnsSlashCommandsIntoOneStepWithoutTheModel(t *testing.T) {
	model := &plannerTestModel{content: searchThenCompare}
	app := &Application{model: model}
	plan, _ := app.planTurn(context.Background(), domain.User{}, domain.Conversation{}, qq.IncomingMessage{Content: "/use 2"}, parseCommand("/use 2"), 1)
	if plan.Route != "plan" || len(plan.Steps) != 1 || plan.Steps[0].Tool != "switch_conversation" || string(plan.Steps[0].Args) != `{"conversation_no":2}` {
		t.Fatalf("unexpected command plan: %+v", plan)
	}
	if model.calls != 0 {
		t.Fatal("slash commands must not reach the planner")
	}
}

// planExecTestStore persists searched papers in memory so a later step can read them.
type planExecTestStore struct {
	dao.Store
	papers map[uint64]domain.Paper
	nextID uint64
}

func (s *planExecTestStore) ListRecentUserMessages(context.Context, uint64, int) ([]domain.Message, error) {
	return nil, nil
}
func (s *planExecTestStore) UpsertPaper(_ context.Context, paper domain.Paper) (domain.Paper, error) {
	for _, stored := range s.papers {
		if stored.ExternalID == paper.ExternalID {
			return stored, nil
		}
	}
	s.nextID++
	paper.ID = 100 + s.nextID
	s.papers[paper.ID] = paper
	return paper, nil
}
func (s *planExecTestStore) GetPaper(_ context.Context, id uint64) (domain.Paper, error) {
	paper, ok := s.papers[id]
	if !ok {
		return domain.Paper{}, dao.ErrNotFound
	}
	return paper, nil
}

type planExecTestSource struct {
	PaperSource
	found   int
	request domain.PaperSearchRequest
}

func (s *planExecTestSource) Search(_ context.Context, request domain.PaperSearchRequest) ([]domain.Candidate, error) {
	s.request = request
	candidates := make([]domain.Candidate, 0, s.found)
	for index := 1; index <= s.found; index++ {
		abstract := fmt.Sprintf("Abstract of paper %d.", index)
		candidates = append(candidates, domain.Candidate{Paper: domain.Paper{
			Source: domain.PaperSourceArxiv, ExternalID: fmt.Sprintf("2401.0000%d", index),
			Title: fmt.Sprintf("Paper %d", index), Abstract: &abstract,
		}})
	}
	return candidates, nil
}

func planExecApp(found int) (*Application, *planExecTestSource) {
	source := &planExecTestSource{found: found}
	return &Application{store: &planExecTestStore{papers: map[uint64]domain.Paper{}}, papers: source, model: unavailableModel{}}, source
}

func newPlanConversation() domain.Conversation {
	current, _ := json.Marshal(newSearchContext("默认会话"))
	return domain.Conversation{ID: 4, ConversationNo: 1, Title: "默认会话", CurrentContext: current}
}

func eventsOfKind(events []recordedDelivery, kind string) []recordedDelivery {
	var found []recordedDelivery
	for _, event := range events {
		if event.Kind == kind {
			found = append(found, event)
		}
	}
	return found
}

func TestExecutePlanFeedsTheDisplayedSearchListIntoTheNextStep(t *testing.T) {
	app, source := planExecApp(3)
	recorder := &recordingDeliverer{}
	input := qq.IncomingMessage{MessageID: "m1", Content: "找 3 篇联邦学习论文，然后查看第二篇的完整摘要"}
	steps := []planStep{searchStep(7), selectStep("get_paper_abstract", domain.PlanSelector{Kind: "position", Positions: []int{2}}, nil)}

	traces, err := app.executePlan(context.Background(), domain.User{}, newPlanConversation(), input, steps, recorder)
	if err != nil {
		t.Fatalf("executePlan: %v", err)
	}
	if source.request.Limit != 3 {
		t.Fatalf("search count must come from the user's words before the step boundary, got %d", source.request.Limit)
	}
	search := eventsOfKind(recorder.events, "search")
	streams := eventsOfKind(recorder.events, "stream")
	if len(search) != 1 || len(search[0].Shown) != 3 || len(streams) != 1 {
		t.Fatalf("want one candidate list then one abstract, got %+v", recorder.events)
	}
	second := search[0].Shown[1]
	if !strings.Contains(streams[0].Text, fmt.Sprintf("#%d《%s》", second.Ordinal, second.Title)) {
		t.Fatalf("abstract step did not read the second displayed paper %+v: %q", second, streams[0].Text)
	}
	notes := eventsOfKind(recorder.events, "text")
	if len(notes) == 0 || !strings.Contains(notes[len(notes)-1].Text, "相关性未判断") {
		t.Fatalf("a selected paper without a relevance judgment must be called out, got %+v", notes)
	}
	if traces[0].Status != "succeeded" || traces[1].Status != "succeeded" || !strings.Contains(string(traces[1].Args), `"ordinal":`) {
		t.Fatalf("unexpected step traces: %+v", traces)
	}
}

func TestExecutePlanStopsWhenTooFewPapersWereDisplayed(t *testing.T) {
	app, _ := planExecApp(1)
	recorder := &recordingDeliverer{}
	steps := []planStep{searchStep(5), selectStep("compare_papers", top(2), nil)}
	traces, err := app.executePlan(context.Background(), domain.User{}, newPlanConversation(), qq.IncomingMessage{MessageID: "m2", Content: "找论文然后对比最相关的两篇"}, steps, recorder)
	if err != nil {
		t.Fatalf("too few papers is an expected stop, not a failure: %v", err)
	}
	last := recorder.events[len(recorder.events)-1]
	if last.Kind != "text" || !strings.Contains(last.Text, "只展示了 1 篇") {
		t.Fatalf("missing stop explanation: %+v", recorder.events)
	}
	if len(eventsOfKind(recorder.events, "search")) != 1 {
		t.Fatal("the completed search must still be delivered")
	}
	if traces[0].Status != "succeeded" || traces[1].Status != "stopped" {
		t.Fatalf("unexpected step traces: %+v", traces)
	}
}

func TestExecutePlanSaysWhichStepsDidNotRunAfterAStoppedStep(t *testing.T) {
	app, _ := planExecApp(0)
	recorder := &recordingDeliverer{}
	steps := []planStep{searchStep(5), selectStep("get_paper_abstract", top(1), nil)}
	traces, err := app.executePlan(context.Background(), domain.User{}, newPlanConversation(), qq.IncomingMessage{MessageID: "m3", Content: "找论文，然后看最相关那篇的摘要"}, steps, recorder)
	if err != nil {
		t.Fatalf("no results is an expected stop: %v", err)
	}
	last := recorder.events[len(recorder.events)-1]
	if !strings.Contains(last.Text, "以下步骤没有执行：查看摘要") {
		t.Fatalf("missing not-run note: %+v", recorder.events)
	}
	if traces[0].Status != "stopped" || traces[1].Status != "skipped" {
		t.Fatalf("unexpected step traces: %+v", traces)
	}
}

// webPlanTestStore is the Web turn store plus enough paper persistence to run a two-step plan.
type webPlanTestStore struct {
	*webTestStore
	exec          *planExecTestStore
	searchResults int
}

func (s *webPlanTestStore) ListRecentUserMessages(ctx context.Context, id uint64, limit int) ([]domain.Message, error) {
	return s.exec.ListRecentUserMessages(ctx, id, limit)
}
func (s *webPlanTestStore) UpsertPaper(ctx context.Context, paper domain.Paper) (domain.Paper, error) {
	return s.exec.UpsertPaper(ctx, paper)
}
func (s *webPlanTestStore) GetPaper(ctx context.Context, id uint64) (domain.Paper, error) {
	return s.exec.GetPaper(ctx, id)
}
func (s *webPlanTestStore) FindLatestShownMessage(context.Context, uint64) (domain.Message, error) {
	return domain.Message{}, dao.ErrNotFound
}
func (s *webPlanTestStore) RenameProjectConversation(_ context.Context, _ domain.ProjectOwner, _ uint32, number uint32, title string) (domain.Conversation, error) {
	return domain.Conversation{ConversationNo: number, Title: title}, nil
}
func (s *webPlanTestStore) SaveProjectSearchResult(_ context.Context, _ domain.ProjectOwner, _ uint32, _ uint32, _ json.RawMessage, message domain.Message) error {
	s.searchResults++
	_, err := s.InsertMessage(context.Background(), message)
	return err
}

func TestRunWebTurnRunsATwoStepPlanWithStepProgressAndATrace(t *testing.T) {
	store := &webPlanTestStore{webTestStore: &webTestStore{}, exec: &planExecTestStore{papers: map[uint64]domain.Paper{}}}
	planner := &plannerTestModel{content: `{"route":"plan","outcome":"检索并查看摘要","response":"","confidence":0.9,"steps":[
		{"tool":"search_papers","args":{"query":"联邦学习","count":5,"mode":"restart","exclude_seen":false}},
		{"tool":"get_paper_abstract","args":{},"select":{"kind":"top_relevance","count":1}}]}`}
	app := New(store, &planExecTestSource{found: 2}, planner, nil, nil)
	var progress []string
	err := app.RunWebTurn(context.Background(), WebActor{Type: WebActorAccount, ID: 7}, "找 2 篇联邦学习论文，然后看最相关那篇的完整摘要", func(event WebEvent) error {
		if event.Status == WebStatusProgress {
			progress = append(progress, event.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebTurn: %v", err)
	}
	joined := strings.Join(progress, " | ")
	if !strings.Contains(joined, "步骤 1/2：论文检索") || !strings.Contains(joined, "步骤 2/2：读取摘要") {
		t.Fatalf("missing step progress labels: %s", joined)
	}
	if store.searchResults != 1 {
		t.Fatalf("want one persisted candidate list, got %d", store.searchResults)
	}
	trace := store.trace
	if trace.Version != agentTraceVersion || trace.Route != "plan" || trace.Tool != "search_papers" || len(trace.Plan) != 2 {
		t.Fatalf("unexpected trace: %+v", trace)
	}
	if trace.Plan[0].Status != "succeeded" || trace.Plan[1].Status != "succeeded" || trace.Status != "succeeded" {
		t.Fatalf("unexpected step statuses: %+v", trace.Plan)
	}
}

func TestHeavyStepKeywordsIgnoreAdverbsAndNegations(t *testing.T) {
	steps := []planStep{searchStep(5), selectStep("compare_papers", top(2), nil)}
	for _, text := range []string{
		"找几篇比较新的联邦学习论文，快速精读第一篇", // "比较" as "fairly"
		"找 5 篇论文，不需要对比，快速精读第一篇",
		"找 5 篇论文，无需对比",
	} {
		if err := validatePlan(steps, text, domain.SearchContext{}, maxPlanSteps); err == nil {
			t.Errorf("%q must not authorize a comparison", text)
		}
	}
	for _, text := range []string{"找 5 篇论文，比较一下最相关的两篇", "找 5 篇论文然后对比前两篇"} {
		if err := validatePlan(steps, text, domain.SearchContext{}, maxPlanSteps); err != nil {
			t.Errorf("%q should authorize a comparison: %v", text, err)
		}
	}
}

func TestExecutePlanChoosesTheSourceFromTheWholeMessage(t *testing.T) {
	app, source := planExecApp(2)
	steps := []planStep{searchStep(5), selectStep("get_paper_abstract", top(1), nil)}
	input := qq.IncomingMessage{MessageID: "m4", Content: "找联邦学习论文，只要 IEEE 的，然后看最相关那篇的摘要"}
	if _, err := app.executePlan(context.Background(), domain.User{}, newPlanConversation(), input, steps, &recordingDeliverer{}); err != nil {
		t.Fatalf("executePlan: %v", err)
	}
	if source.request.Spec.EffectiveSource() != domain.PaperSourceIEEE {
		t.Fatalf("IEEE named after the first comma must still select IEEE, got %q", source.request.Spec.EffectiveSource())
	}
}

// refusingGate lets the first tool through and refuses every later one with err.
type refusingGate struct {
	calls int
	err   error
}

func (g *refusingGate) BeforeTool(context.Context, string) (func(error) error, error) {
	g.calls++
	if g.calls > 1 {
		return nil, g.err
	}
	return nil, nil
}

func TestExecutePlanStopsWithAnExplanationWhenALaterStepRunsOutOfQuota(t *testing.T) {
	app, _ := planExecApp(2)
	recorder := &recordingDeliverer{}
	ctx := withToolGate(context.Background(), &refusingGate{err: dao.ErrGuestDeepReadExhausted})
	steps := []planStep{searchStep(5), selectStep("read_paper_quick", top(1), nil)}
	traces, err := app.executePlan(ctx, domain.User{}, newPlanConversation(), qq.IncomingMessage{MessageID: "m5", Content: "找论文，然后快速精读第一篇"}, steps, recorder)
	if err != nil {
		t.Fatalf("a quota refusal after a delivered step is a stop, not a failure: %v", err)
	}
	last := recorder.events[len(recorder.events)-1]
	if !strings.Contains(last.Text, "精读资格已使用") || !strings.Contains(last.Text, "以下步骤没有执行：快速精读") {
		t.Fatalf("missing quota explanation: %q", last.Text)
	}
	if traces[0].Status != "succeeded" || traces[1].Status != "stopped" || traces[1].ErrorCode != "GUEST_DEEP_READ_EXHAUSTED" {
		t.Fatalf("unexpected step traces: %+v", traces)
	}
}

type failingSearchSource struct{ PaperSource }

func (failingSearchSource) Search(context.Context, domain.PaperSearchRequest) ([]domain.Candidate, error) {
	return nil, errors.New("sidecar unavailable")
}

func TestExecutePlanNamesTheStepsSkippedAfterAFailure(t *testing.T) {
	app := &Application{store: &planExecTestStore{papers: map[uint64]domain.Paper{}}, papers: failingSearchSource{}, model: unavailableModel{}}
	recorder := &recordingDeliverer{}
	steps := []planStep{searchStep(5), selectStep("read_paper_quick", top(1), nil)}
	traces, err := app.executePlan(context.Background(), domain.User{}, newPlanConversation(), qq.IncomingMessage{MessageID: "m6", Content: "找论文，然后快速精读第一篇"}, steps, recorder)
	if err == nil {
		t.Fatal("a failed search must fail the task")
	}
	last := recorder.events[len(recorder.events)-1]
	if !strings.Contains(last.Text, "以下步骤没有执行：快速精读") {
		t.Fatalf("missing not-run note after a failure: %+v", recorder.events)
	}
	if traces[0].Status != "failed" || traces[1].Status != "skipped" {
		t.Fatalf("unexpected step traces: %+v", traces)
	}
}
