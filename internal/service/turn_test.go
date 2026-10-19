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

// recordedDelivery is one captured emission on the deliverer seam.
type recordedDelivery struct {
	Kind           string // "text" | "stream" | "read" | "search"
	Text           string
	Shown          []domain.ShownPaper
	ConversationID uint64
}

// recordingDeliverer is a channel-neutral deliverer used by tests: it records what a tool tried
// to send instead of talking to QQ or SSE. It proves tools emit through the seam rather than
// reaching for a channel directly.
type recordingDeliverer struct{ events []recordedDelivery }

func (r *recordingDeliverer) Text(_ context.Context, conv domain.Conversation, text string, shown []domain.ShownPaper, _ float64) error {
	r.events = append(r.events, recordedDelivery{Kind: "text", Text: text, Shown: shown, ConversationID: conv.ID})
	return nil
}
func (r *recordingDeliverer) Stream(_ context.Context, conv domain.Conversation, text string, _ float64) error {
	r.events = append(r.events, recordedDelivery{Kind: "stream", Text: text, ConversationID: conv.ID})
	return nil
}
func (r *recordingDeliverer) Read(_ context.Context, conv domain.Conversation, _ uint64, _, text string, _ float64) error {
	r.events = append(r.events, recordedDelivery{Kind: "read", Text: text, ConversationID: conv.ID})
	return nil
}
func (r *recordingDeliverer) SearchResult(_ context.Context, conv domain.Conversation, reply searchReply, _ float64) error {
	r.events = append(r.events, recordedDelivery{Kind: "search", Text: reply.QQ.Text, Shown: reply.QQ.Shown, ConversationID: conv.ID})
	return nil
}

// The get_current_conversation tool takes no store, model or messenger: it should route a single
// Text emission carrying the conversation header through the seam. This is the smallest proof
// that executeToolCall dispatches output to the deliverer, not to a QQ-specific path.
func TestExecuteToolCallRoutesCurrentConversationThroughSeam(t *testing.T) {
	app := &Application{}
	rec := &recordingDeliverer{}
	conv := domain.Conversation{ID: 7, ConversationNo: 2, Title: "扩散模型"}

	err := app.executeToolCall(context.Background(), domain.User{}, conv, qq.IncomingMessage{},
		deepseek.ToolCall{Name: "get_current_conversation", Arguments: "{}"}, rec)
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if len(rec.events) != 1 || rec.events[0].Kind != "text" {
		t.Fatalf("expected one text emission, got %+v", rec.events)
	}
	if rec.events[0].ConversationID != 7 || !strings.Contains(rec.events[0].Text, "会话 #2") {
		t.Fatalf("emission did not carry the current conversation: %+v", rec.events[0])
	}
}

// An empty search query short-circuits to help text through the seam before any store or lock is
// touched, so it exercises the search tool's delivery routing with a nil Application.
func TestSearchEmptyQueryRoutesHelpThroughSeam(t *testing.T) {
	app := &Application{}
	rec := &recordingDeliverer{}
	conv := domain.Conversation{ID: 3, ConversationNo: 1, Title: "x"}

	if err := app.search(context.Background(), domain.User{}, conv, qq.IncomingMessage{}, "", searchToolPlan{}, rec); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rec.events) != 1 || rec.events[0].Kind != "text" || rec.events[0].Text != helpText() {
		t.Fatalf("expected help text emission, got %+v", rec.events)
	}
}

// An unauthorized tool name is rejected without emitting anything to the user.
func TestExecuteToolCallRejectsUnknownToolWithoutEmitting(t *testing.T) {
	app := &Application{}
	rec := &recordingDeliverer{}
	err := app.executeToolCall(context.Background(), domain.User{}, domain.Conversation{ID: 1}, qq.IncomingMessage{},
		deepseek.ToolCall{Name: "definitely_not_a_tool", Arguments: "{}"}, rec)
	if err == nil {
		t.Fatal("expected an error for an unauthorized tool")
	}
	if len(rec.events) != 0 {
		t.Fatalf("unauthorized tool should not emit, got %+v", rec.events)
	}
}

type projectToolTestStore struct {
	dao.Store
	owner         domain.ProjectOwner
	projectNo     uint32
	paperIDs      []uint64
	getPaperIDs   []uint64
	abstract      string
	nextMessageID uint64
	messages      []domain.Message
	// privateText is account-private full text keyed by "accountID/paperID".
	privateText map[string]string
}

func (s *projectToolTestStore) GetAccountFullText(_ context.Context, accountID, paperID uint64) (string, error) {
	if text, ok := s.privateText[fmt.Sprintf("%d/%d", accountID, paperID)]; ok {
		return text, nil
	}
	return "", dao.ErrNotFound
}

func (s *projectToolTestStore) InsertMessage(_ context.Context, message domain.Message) (domain.Message, error) {
	s.nextMessageID++
	message.ID = s.nextMessageID
	s.messages = append(s.messages, message)
	return message, nil
}

func (s *projectToolTestStore) UpdateMessageContent(_ context.Context, id uint64, content string, cost float64) error {
	for index := range s.messages {
		if s.messages[index].ID == id {
			s.messages[index].Content = &content
			s.messages[index].CostUSD = cost
			return nil
		}
	}
	return dao.ErrNotFound
}

func (s *projectToolTestStore) UpdateMessageDelivery(_ context.Context, id uint64, status, _ string) error {
	for index := range s.messages {
		if s.messages[index].ID == id {
			s.messages[index].DeliveryStatus = status
			return nil
		}
	}
	return dao.ErrNotFound
}

func (s *projectToolTestStore) ListCurrentProjectPapers(_ context.Context, owner domain.ProjectOwner, projectNo uint32, paperIDs []uint64, _ int) ([]domain.ProjectPaper, error) {
	s.owner, s.projectNo = owner, projectNo
	s.paperIDs = append([]uint64(nil), paperIDs...)
	if len(paperIDs) == 0 {
		return []domain.ProjectPaper{{PaperID: 11, Title: "Project paper", Status: domain.ProjectPaperStatusActive}}, nil
	}
	items := make([]domain.ProjectPaper, 0, len(paperIDs))
	for _, paperID := range paperIDs {
		items = append(items, domain.ProjectPaper{PaperID: paperID, Title: fmt.Sprintf("Project paper %d", paperID), Status: domain.ProjectPaperStatusActive})
	}
	return items, nil
}

func (s *projectToolTestStore) ListProjectReadResults(_ context.Context, _ domain.ProjectOwner, _ uint32, paperID uint64, _ int, _ *domain.PagePosition) ([]domain.ProjectReadResult, *domain.PagePosition, error) {
	if paperID != 11 {
		return nil, nil, dao.ErrProjectPaperNotFound
	}
	return []domain.ProjectReadResult{{ReadType: domain.ProjectReadTypeQuick, SourceConversationNo: 2, Content: "old model output"}}, nil, nil
}

func (s *projectToolTestStore) GetPaper(_ context.Context, paperID uint64) (domain.Paper, error) {
	s.getPaperIDs = append(s.getPaperIDs, paperID)
	abstract := s.abstract
	if abstract == "" {
		abstract = "fresh abstract evidence"
	}
	return domain.Paper{ID: paperID, Title: fmt.Sprintf("Project paper %d", paperID), Abstract: &abstract}, nil
}

type projectToolTestModel struct {
	request       deepseek.Request
	streamRequest deepseek.Request
	streamDeltas  []string
	completeCalls int
}

func (m *projectToolTestModel) Available() bool { return true }
func (m *projectToolTestModel) Complete(_ context.Context, request deepseek.Request) (string, domain.ModelUsage, error) {
	m.completeCalls++
	m.request = request
	return "evidence-based answer", domain.ModelUsage{CostUSD: 0.1}, nil
}
func (m *projectToolTestModel) CallTool(context.Context, deepseek.ToolRequest) (deepseek.ToolCall, domain.ModelUsage, error) {
	return deepseek.ToolCall{}, domain.ModelUsage{}, errors.New("not used")
}
func (m *projectToolTestModel) Stream(_ context.Context, request deepseek.Request, onDelta func(string) error) (domain.ModelUsage, error) {
	m.streamRequest = request
	if m.streamDeltas == nil {
		return domain.ModelUsage{}, errors.New("not used")
	}
	for _, delta := range m.streamDeltas {
		if err := onDelta(delta); err != nil {
			return domain.ModelUsage{}, err
		}
	}
	return domain.ModelUsage{CostUSD: 0.1}, nil
}

func TestProjectResearchToolsRequireWebScopeAndExcludeTargetSelectors(t *testing.T) {
	projectTools := func(ctx context.Context) []deepseek.ToolDefinition {
		var found []deepseek.ToolDefinition
		for _, definition := range toolCatalogue(ctx) {
			if strings.Contains(definition.Function.Name, "project") {
				found = append(found, definition)
			}
		}
		return found
	}
	if tools := projectTools(context.Background()); len(tools) != 0 {
		t.Fatalf("QQ/no-scope context received project tools: %+v", tools)
	}
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: 7}
	ctx := withProjectAgentScope(context.Background(), owner, 3)
	tools := projectTools(ctx)
	if len(tools) != 3 {
		t.Fatalf("expected exactly three Web project tools, got %d", len(tools))
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	schema := string(encoded)
	for _, forbidden := range []string{"projectId", "project_id", "owner", "path", "url"} {
		if strings.Contains(schema, forbidden) {
			t.Fatalf("project tool schema exposed forbidden selector %q: %s", forbidden, schema)
		}
	}
	for _, name := range []string{"list_current_project_papers", "get_current_project_paper_read_history", "answer_from_current_project_papers"} {
		if !strings.Contains(schema, name) {
			t.Fatalf("missing project tool %q", name)
		}
	}
}

func TestProjectResearchToolsUseInjectedScopeAndMarkHistoryUntrusted(t *testing.T) {
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerGuest, ID: 8}
	ctx := withProjectAgentScope(context.Background(), owner, 4)
	store := &projectToolTestStore{}
	app := &Application{store: store}
	recorder := &recordingDeliverer{}
	conversation := domain.Conversation{ID: 9, ConversationNo: 1}

	if err := app.executeToolCall(ctx, domain.User{}, conversation, qq.IncomingMessage{}, deepseek.ToolCall{Name: "list_current_project_papers", Arguments: `{"limit":10}`}, recorder); err != nil {
		t.Fatalf("list project papers: %v", err)
	}
	if store.owner != owner || store.projectNo != 4 || len(store.paperIDs) != 0 {
		t.Fatalf("list did not use injected scope: owner=%+v project=%d ids=%v", store.owner, store.projectNo, store.paperIDs)
	}
	if len(recorder.events) != 1 || !strings.Contains(recorder.events[0].Text, "可能与原文有出入") {
		t.Fatalf("list output missing history warning: %+v", recorder.events)
	}

	recorder.events = nil
	if err := app.executeToolCall(ctx, domain.User{}, conversation, qq.IncomingMessage{}, deepseek.ToolCall{Name: "get_current_project_paper_read_history", Arguments: `{"paper_id":11,"limit":20}`}, recorder); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(store.paperIDs) != 1 || store.paperIDs[0] != 11 {
		t.Fatalf("history did not validate active project relation: %v", store.paperIDs)
	}
	if len(recorder.events) != 1 || !strings.Contains(recorder.events[0].Text, "（过去生成的分析，可能与原文有出入") {
		t.Fatalf("history output was not marked untrusted: %+v", recorder.events)
	}
}

func TestWebProjectResearchStreamsModelDeltas(t *testing.T) {
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: 10}
	ctx := withProjectAgentScope(context.Background(), owner, 5)
	store := &projectToolTestStore{}
	model := &projectToolTestModel{streamDeltas: []string{"evidence-", "based answer"}}
	app := New(store, nil, model, nil, nil)
	var events []WebEvent
	deliver := &webDeliverer{app: app, emit: func(event WebEvent) error {
		events = append(events, event)
		return nil
	}}

	err := app.answerFromCurrentProjectPapers(ctx, domain.Conversation{ID: 9}, qq.IncomingMessage{MessageID: "web-stream"}, []uint64{11}, "What is supported?", deliver)
	if err != nil {
		t.Fatalf("answer from project papers: %v", err)
	}
	if model.completeCalls != 0 {
		t.Fatalf("Web project research used Complete %d times instead of Stream", model.completeCalls)
	}
	if len(events) != 2 || events[0].Status != WebStatusDelta || events[0].Content != "evidence-" || events[1].Status != WebStatusDelta || events[1].Content != "based answer" {
		t.Fatalf("want separate streamed deltas, got %+v", events)
	}
	if len(store.messages) != 1 || store.messages[0].Content == nil || *store.messages[0].Content != "evidence-based answer" || store.messages[0].DeliveryStatus != "sent" {
		t.Fatalf("streamed answer was not persisted as sent: %+v", store.messages)
	}
}

func TestAnswerFromCurrentProjectPapersRereadsEvidenceAndStreamsResult(t *testing.T) {
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: 10}
	ctx := withProjectAgentScope(context.Background(), owner, 5)
	store := &projectToolTestStore{}
	model := &projectToolTestModel{}
	app := &Application{store: store, model: model}
	recorder := &recordingDeliverer{}
	input := qq.IncomingMessage{MessageID: "web-test"}
	call := deepseek.ToolCall{Name: "answer_from_current_project_papers", Arguments: `{"paper_ids":[11],"question":"What is supported?"}`}

	if err := app.executeToolCall(ctx, domain.User{}, domain.Conversation{ID: 9}, input, call, recorder); err != nil {
		t.Fatalf("answer from project papers: %v", err)
	}
	if len(recorder.events) != 1 || recorder.events[0].Kind != "stream" || recorder.events[0].Text != "evidence-based answer" {
		t.Fatalf("answer was not delivered through Stream: %+v", recorder.events)
	}
	requestJSON, _ := json.Marshal(model.request)
	if !strings.Contains(string(requestJSON), "fresh abstract evidence") || strings.Contains(string(requestJSON), "old model output") {
		t.Fatalf("answer did not use freshly read evidence exclusively: %s", requestJSON)
	}
}

func TestAnswerFromCurrentProjectPapersReadsEverySelectedPaperWithinBudget(t *testing.T) {
	owner := domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: 12}
	ctx := withProjectAgentScope(context.Background(), owner, 6)
	store := &projectToolTestStore{abstract: strings.Repeat("evidence ", 20_000)}
	model := &projectToolTestModel{}
	app := &Application{store: store, model: model}
	recorder := &recordingDeliverer{}
	paperIDs := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	if err := app.answerFromCurrentProjectPapers(ctx, domain.Conversation{ID: 9}, qq.IncomingMessage{MessageID: "web-budget"}, paperIDs, "Compare all selected papers", recorder); err != nil {
		t.Fatalf("answer from all selected project papers: %v", err)
	}
	if len(store.getPaperIDs) != len(paperIDs) {
		t.Fatalf("expected every selected paper to be reread, got %v", store.getPaperIDs)
	}
	requestJSON, _ := json.Marshal(model.request)
	if !strings.Contains(string(requestJSON), "论文 ID 10 ") {
		t.Fatalf("last selected paper was omitted from bounded evidence: %s", boundedRunes(string(requestJSON), 500, "…"))
	}
}
