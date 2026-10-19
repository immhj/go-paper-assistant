package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"paperassistant/internal/dao"
	"paperassistant/internal/dao/daotest"
	"paperassistant/internal/domain"
	guardprovider "paperassistant/internal/provider/guard"
)

type webTestStore struct {
	dao.Store
	activeAccountCalls int
	activeGuestCalls   int
	createAccountCalls int
	createGuestCalls   int
	nextMessageID      uint64
	messages           []domain.Message
	traceMessageID     uint64
	trace              domain.AgentTrace
	traceCost          float64
	insertBotErr       error
	commitErr          error
	failCommitAttempts int
	commitAttempts     int
	platform           dao.PlatformStore
	commits            []domain.ProjectReadCommit
	failedMessageIDs   []uint64
}

func (s *webTestStore) EnsureActiveProject(_ context.Context, owner domain.ProjectOwner, _ bool, current json.RawMessage) (domain.Project, *domain.Conversation, bool, error) {
	projectID := 100 + owner.ID
	conversationID := uint64(11)
	if owner.IsAccount() {
		s.activeAccountCalls++
		s.createAccountCalls++
	} else {
		s.activeGuestCalls++
		s.createGuestCalls++
		conversationID = 22
	}
	conversation := domain.Conversation{ID: conversationID, ProjectID: &projectID, ConversationNo: 1, Title: "默认会话", CurrentContext: current, Status: domain.ConversationStatusActive}
	project := domain.Project{ID: projectID, ProjectNo: 1, Title: "默认项目", Status: domain.ProjectStatusActive, ActiveConversationID: &conversation.ID}
	return project, &conversation, true, nil
}
func (s *webTestStore) GetActiveAccountConversation(context.Context, uint64) (domain.Conversation, error) {
	s.activeAccountCalls++
	return domain.Conversation{}, dao.ErrNotFound
}
func (s *webTestStore) GetActiveGuestConversation(context.Context, uint64) (domain.Conversation, error) {
	s.activeGuestCalls++
	return domain.Conversation{}, dao.ErrNotFound
}
func (s *webTestStore) CreateAccountConversation(_ context.Context, id uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	s.createAccountCalls++
	return domain.Conversation{ID: 11, ProjectID: &id, ConversationNo: 1, Title: title, CurrentContext: current}, nil
}
func (s *webTestStore) CreateGuestConversation(_ context.Context, id uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	s.createGuestCalls++
	return domain.Conversation{ID: 22, ProjectID: &id, ConversationNo: 1, Title: title, CurrentContext: current}, nil
}
func (s *webTestStore) InsertProjectMessage(_ context.Context, _ domain.ProjectOwner, _ uint32, _ uint32, message domain.Message) (domain.Message, error) {
	return s.InsertMessage(context.Background(), message)
}
func (s *webTestStore) InsertMessage(_ context.Context, message domain.Message) (domain.Message, error) {
	if message.Role == "bot" && s.insertBotErr != nil {
		return domain.Message{}, s.insertBotErr
	}
	s.nextMessageID++
	message.ID = s.nextMessageID
	s.messages = append(s.messages, message)
	return message, nil
}
func (s *webTestStore) UpdateMessageAgentTraceByID(_ context.Context, id uint64, raw json.RawMessage, cost float64) error {
	s.traceMessageID = id
	s.traceCost = cost
	return json.Unmarshal(raw, &s.trace)
}
func (s *webTestStore) UpdateMessageContent(_ context.Context, id uint64, content string, cost float64) error {
	for index := range s.messages {
		if s.messages[index].ID == id {
			s.messages[index].Content = &content
			s.messages[index].CostUSD = cost
			return nil
		}
	}
	return dao.ErrNotFound
}
func (s *webTestStore) FailReservedMessage(_ context.Context, id uint64) error {
	s.failedMessageIDs = append(s.failedMessageIDs, id)
	for index := range s.messages {
		if s.messages[index].ID == id && s.messages[index].DeliveryStatus == "reserved" {
			s.messages[index].DeliveryStatus = "failed"
		}
	}
	return nil
}
func (s *webTestStore) CommitProjectReadResult(ctx context.Context, commit domain.ProjectReadCommit) (domain.ProjectReadResult, error) {
	s.commitAttempts++
	if s.commitErr != nil && (s.failCommitAttempts == 0 || s.commitAttempts <= s.failCommitAttempts) {
		return domain.ProjectReadResult{}, s.commitErr
	}
	if commit.GuestReservationID != nil {
		if s.platform == nil {
			return domain.ProjectReadResult{}, errors.New("missing fake guest platform")
		}
		if err := s.platform.FinishGuestDeepRead(ctx, commit.ProjectOwner.ID, *commit.GuestReservationID, true); err != nil {
			return domain.ProjectReadResult{}, err
		}
	}
	for index := range s.messages {
		if s.messages[index].ID == commit.MessageID {
			s.messages[index].Content = &commit.Content
			s.messages[index].CostUSD = commit.CostUSD
			s.messages[index].DeliveryStatus = "sent"
		}
	}
	s.commits = append(s.commits, commit)
	return domain.ProjectReadResult{MessageID: commit.MessageID, ReadType: commit.ReadType, Content: commit.Content}, nil
}

func TestRunWebTurnSelectsAccountAndGuestConversations(t *testing.T) {
	for _, test := range []struct {
		name  string
		actor WebActor
		check func(*testing.T, *webTestStore)
	}{
		{name: "account", actor: WebActor{Type: WebActorAccount, ID: 7, IP: "192.0.2.1"}, check: func(t *testing.T, store *webTestStore) {
			if store.activeAccountCalls != 1 || store.createAccountCalls != 1 || store.activeGuestCalls != 0 {
				t.Fatalf("wrong account selection: %+v", store)
			}
		}},
		{name: "guest", actor: WebActor{Type: WebActorGuest, ID: 8, IP: "192.0.2.2"}, check: func(t *testing.T, store *webTestStore) {
			if store.activeGuestCalls != 1 || store.createGuestCalls != 1 || store.activeAccountCalls != 0 {
				t.Fatalf("wrong guest selection: %+v", store)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &webTestStore{}
			app := New(store, nil, nil, nil, nil)
			var events []WebEvent
			err := app.RunWebTurn(context.Background(), test.actor, "/help", func(event WebEvent) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatalf("RunWebTurn: %v", err)
			}
			test.check(t, store)
			if len(events) < 4 || events[0].Status != WebStatusStarted || events[1].Status != WebStatusProgress || events[len(events)-1].Status != WebStatusCompleted {
				t.Fatalf("unexpected Web turn envelope: %+v", events)
			}
			var output strings.Builder
			for _, event := range events[2 : len(events)-1] {
				if event.Status != WebStatusDelta {
					t.Fatalf("tool output must use delta frames, got %+v", events)
				}
				output.WriteString(event.Content)
			}
			if output.String() != helpText() {
				t.Fatalf("delta content did not reconstruct help text: %q", output.String())
			}
			if store.traceMessageID == 0 || store.trace.Status != "succeeded" {
				t.Fatalf("trace not persisted: %+v", store.trace)
			}
		})
	}
}

func TestRunWebTurnRejectsZeroActorID(t *testing.T) {
	app := New(&webTestStore{}, nil, nil, nil, nil)
	var event WebEvent
	err := app.RunWebTurn(context.Background(), WebActor{Type: WebActorGuest}, "/help", func(value WebEvent) error {
		event = value
		return nil
	})
	if err == nil || event.Status != WebStatusError {
		t.Fatalf("want actor error, got err=%v event=%+v", err, event)
	}
}

func TestGuestQuickAndDeepShareOneReservation(t *testing.T) {
	platform := daotest.NewFakePlatformStore()
	guest, err := platform.CreateGuestSession(context.Background(), make([]byte, 32))
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	store := &webTestStore{platform: platform}
	app := New(store, nil, nil, nil, nil, WithPlatformStore(platform))
	var events []WebEvent
	deliver := &webDeliverer{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, projectNo: 1, emit: func(event WebEvent) error {
		events = append(events, event)
		return nil
	}}
	gate := &webToolGate{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, deliver: deliver}
	finish, err := gate.BeforeTool(context.Background(), "read_paper_quick")
	if err != nil {
		t.Fatalf("reserve quick: %v", err)
	}
	if err = deliver.Read(context.Background(), domain.Conversation{ID: 1, ConversationNo: 1}, 9, domain.ProjectReadTypeQuick, "valid result", 0.1); err != nil {
		t.Fatalf("commit quick: %v", err)
	}
	if len(events) != 2 || events[0].Status != WebStatusCommitted || events[0].ProjectNo != 1 || events[0].ConversationNo != 1 || events[0].PaperID != 9 || events[0].ReadType != domain.ProjectReadTypeQuick || events[1].Status != WebStatusDelta || events[1].Content != "valid result" {
		t.Fatalf("missing committed/delta SSE sequence: %+v", events)
	}
	if err = finish(nil); err != nil {
		t.Fatalf("finish quick: %v", err)
	}
	if _, err = gate.BeforeTool(context.Background(), "read_paper_deep"); !errors.Is(err, dao.ErrGuestDeepReadExhausted) {
		t.Fatalf("deep should share exhausted qualification, got %v", err)
	}
}

func TestGuestReadFailureReleasesReservation(t *testing.T) {
	platform := daotest.NewFakePlatformStore()
	guest, _ := platform.CreateGuestSession(context.Background(), make([]byte, 32))
	app := New(&webTestStore{}, nil, nil, nil, nil, WithPlatformStore(platform))
	deliver := &webDeliverer{app: app, emit: func(WebEvent) error { return nil }}
	gate := &webToolGate{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, deliver: deliver}
	finish, err := gate.BeforeTool(context.Background(), "read_paper_deep")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err = finish(errors.New("system failure")); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err = gate.BeforeTool(context.Background(), "read_paper_quick"); err != nil {
		t.Fatalf("qualification was not released: %v", err)
	}
}

func TestGuestReadConsumesAfterPersistenceBeforeEmitFailure(t *testing.T) {
	platform := daotest.NewFakePlatformStore()
	guest, _ := platform.CreateGuestSession(context.Background(), make([]byte, 32))
	store := &webTestStore{platform: platform}
	app := New(store, nil, nil, nil, nil, WithPlatformStore(platform))
	deliver := &webDeliverer{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, projectNo: 1, emit: func(WebEvent) error { return io.ErrClosedPipe }}
	gate := &webToolGate{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, deliver: deliver}
	finish, err := gate.BeforeTool(context.Background(), "read_paper_quick")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	readErr := deliver.Read(context.Background(), domain.Conversation{ID: 1, ConversationNo: 1}, 9, domain.ProjectReadTypeQuick, "valid result", 0.1)
	if !errors.Is(readErr, io.ErrClosedPipe) {
		t.Fatalf("want emit failure, got %v", readErr)
	}
	if err = finish(readErr); err != nil {
		t.Fatalf("emit failure must not release consumed qualification: %v", err)
	}
	if _, err = gate.BeforeTool(context.Background(), "read_paper_deep"); !errors.Is(err, dao.ErrGuestDeepReadExhausted) {
		t.Fatalf("persisted result must consume qualification, got %v", err)
	}
}

func TestWebDelivererSplitsToolOutputIntoDeltas(t *testing.T) {
	const chunkRunes = 160
	content := strings.Repeat("文", chunkRunes+1)
	for _, test := range []struct {
		name    string
		deliver func(*webDeliverer) error
	}{
		{name: "text", deliver: func(deliver *webDeliverer) error {
			return deliver.Text(context.Background(), domain.Conversation{ID: 1}, content, nil, 0)
		}},
		{name: "stream", deliver: func(deliver *webDeliverer) error {
			return deliver.Stream(context.Background(), domain.Conversation{ID: 1}, content, 0)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &webTestStore{}
			var events []WebEvent
			deliver := &webDeliverer{app: &Application{store: store}, emit: func(event WebEvent) error {
				events = append(events, event)
				return nil
			}}
			if err := test.deliver(deliver); err != nil {
				t.Fatalf("deliver tool output: %v", err)
			}
			if len(events) != 2 || events[0].Status != WebStatusDelta || events[1].Status != WebStatusDelta || events[0].Content != strings.Repeat("文", chunkRunes) || events[1].Content != "文" {
				t.Fatalf("want two ordered delta frames, got %+v", events)
			}
		})
	}
}

func TestWebTextDoesNotSwallowPersistenceFailure(t *testing.T) {
	persistErr := errors.New("database unavailable")
	deliver := &webDeliverer{app: &Application{store: &webTestStore{insertBotErr: persistErr}}, emit: func(WebEvent) error {
		t.Fatal("must not emit after persistence failure")
		return nil
	}}
	if err := deliver.Text(context.Background(), domain.Conversation{ID: 1}, "answer", nil, 0); !errors.Is(err, persistErr) {
		t.Fatalf("want persistence error, got %v", err)
	}
}

func TestGuestReadCommitFailureMarksReservedMessageAndReleasesReservation(t *testing.T) {
	platform := daotest.NewFakePlatformStore()
	guest, _ := platform.CreateGuestSession(context.Background(), make([]byte, 32))
	commitErr := errors.New("atomic commit unavailable")
	store := &webTestStore{platform: platform, commitErr: commitErr}
	app := New(store, nil, nil, nil, nil, WithPlatformStore(platform))
	deliver := &webDeliverer{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, projectNo: 1, emit: func(WebEvent) error { return nil }}
	gate := &webToolGate{app: app, actor: WebActor{Type: WebActorGuest, ID: guest.ID}, deliver: deliver}
	finish, err := gate.BeforeTool(context.Background(), "read_paper_deep")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	readErr := deliver.Read(context.Background(), domain.Conversation{ID: 1, ConversationNo: 1}, 9, domain.ProjectReadTypeDeep, "result", 0.1)
	if !errors.Is(readErr, commitErr) {
		t.Fatalf("expected atomic commit failure, got %v", readErr)
	}
	if len(store.failedMessageIDs) != 1 {
		t.Fatalf("reserved message was not marked failed: %+v", store.failedMessageIDs)
	}
	if err = finish(readErr); err != nil {
		t.Fatalf("uncommitted read reservation was not released: %v", err)
	}
	if _, err = gate.BeforeTool(context.Background(), "read_paper_quick"); err != nil {
		t.Fatalf("released qualification cannot be reserved again: %v", err)
	}
}

func TestFinishWebTurnPersistsCompleteTraceCostAndRecordsGuardCost(t *testing.T) {
	store := &webTestStore{}
	webGuard := guardprovider.NewFake(guardprovider.Config{CostHourlyUSD: 0.5, CostDailyUSD: 10})
	app := New(store, nil, nil, nil, nil, WithWebGuard(webGuard))
	trace := domain.AgentTrace{
		Route: "execute", Domain: "paper_analysis", Outcome: "read", RouterSource: "llm",
		RouterUsage: domain.ModelUsage{CostUSD: 0.1}, Tool: "read_paper_quick",
		ExecutorUsage: domain.ModelUsage{CostUSD: 0.1}, ToolUsage: domain.ModelUsage{CostUSD: 0.3},
	}
	if err := app.finishWebTurn(context.Background(), 42, trace, domain.ModelUsage{CostUSD: 0.5}, nil); err != nil {
		t.Fatalf("finishWebTurn: %v", err)
	}
	if store.traceMessageID != 42 || store.traceCost != 0.5 || store.trace.Tool != "read_paper_quick" || store.trace.Status != "succeeded" {
		t.Fatalf("incomplete trace/cost: id=%d cost=%v trace=%+v", store.traceMessageID, store.traceCost, store.trace)
	}
	if _, err := webGuard.BeginTurn(context.Background(), "account:9", "192.0.2.9"); !errors.Is(err, guardprovider.ErrCircuitOpen) {
		t.Fatalf("actual turn cost was not recorded in guard window: %v", err)
	}
}

func TestAccountQuickAndDeepUseTwoPerMinuteReadLimit(t *testing.T) {
	webGuard := guardprovider.NewFake(guardprovider.Config{})
	app := New(&webTestStore{}, nil, nil, nil, nil, WithWebGuard(webGuard))
	gate := &webToolGate{app: app, actor: WebActor{Type: WebActorAccount, ID: 3}, deliver: &webDeliverer{}}
	if _, err := gate.BeforeTool(context.Background(), "read_paper_quick"); err != nil {
		t.Fatalf("first account read: %v", err)
	}
	if _, err := gate.BeforeTool(context.Background(), "read_paper_deep"); err != nil {
		t.Fatalf("second account read: %v", err)
	}
	if _, err := gate.BeforeTool(context.Background(), "read_paper_quick"); !errors.Is(err, guardprovider.ErrRateLimited) {
		t.Fatalf("want account read rate limit, got %v", err)
	}
}

type webConversationManagementStore struct {
	dao.Store
	accountConversations []domain.Conversation
	guestConversations   []domain.Conversation
	activeAccount        domain.Conversation
	activeGuest          domain.Conversation
	createdFor           WebActor
	activatedFor         WebActor
	activatedID          uint64
}

func (s *webConversationManagementStore) owned(owner domain.ProjectOwner) ([]domain.Conversation, domain.Conversation) {
	if owner.IsAccount() {
		return s.accountConversations, s.activeAccount
	}
	return s.guestConversations, s.activeGuest
}
func (s *webConversationManagementStore) GetActiveProjectContext(_ context.Context, owner domain.ProjectOwner) (domain.Project, *domain.Conversation, error) {
	_, active := s.owned(owner)
	if active.ID == 0 {
		return domain.Project{}, nil, dao.ErrNotFound
	}
	project := domain.Project{ID: 100 + owner.ID, ProjectNo: 1, Status: domain.ProjectStatusActive, ActiveConversationID: &active.ID}
	return project, &active, nil
}
func (s *webConversationManagementStore) ListProjectConversations(_ context.Context, owner domain.ProjectOwner, _ uint32, _ string, _ int, _ *domain.PagePosition) ([]domain.Conversation, *domain.PagePosition, error) {
	items, _ := s.owned(owner)
	return items, nil, nil
}
func (s *webConversationManagementStore) CreateProjectConversation(_ context.Context, owner domain.ProjectOwner, _ uint32, title string, current json.RawMessage) (domain.Conversation, error) {
	s.createdFor = WebActor{Type: owner.Type, ID: owner.ID}
	id := uint64(13)
	if owner.IsGuest() {
		id = 23
	}
	projectID := 100 + owner.ID
	return domain.Conversation{ID: id, ProjectID: &projectID, ConversationNo: 3, Title: title, CurrentContext: current, Status: domain.ConversationStatusActive}, nil
}
func (s *webConversationManagementStore) SelectProjectConversation(_ context.Context, owner domain.ProjectOwner, _ uint32, number uint32) (domain.Project, domain.Conversation, error) {
	items, _ := s.owned(owner)
	for _, conversation := range items {
		if conversation.ID == uint64(number) || conversation.ConversationNo == number {
			s.activatedFor = WebActor{Type: owner.Type, ID: owner.ID}
			s.activatedID = conversation.ID
			return domain.Project{ID: 100 + owner.ID, ProjectNo: 1, Status: domain.ProjectStatusActive}, conversation, nil
		}
	}
	return domain.Project{}, domain.Conversation{}, dao.ErrNotFound
}

func (s *webConversationManagementStore) ListAccountConversations(context.Context, uint64, int) ([]domain.Conversation, error) {
	return s.accountConversations, nil
}

func (s *webConversationManagementStore) ListGuestConversations(context.Context, uint64, int) ([]domain.Conversation, error) {
	return s.guestConversations, nil
}

func (s *webConversationManagementStore) GetActiveAccountConversation(context.Context, uint64) (domain.Conversation, error) {
	if s.activeAccount.ID == 0 {
		return domain.Conversation{}, dao.ErrNotFound
	}
	return s.activeAccount, nil
}

func (s *webConversationManagementStore) GetActiveGuestConversation(context.Context, uint64) (domain.Conversation, error) {
	if s.activeGuest.ID == 0 {
		return domain.Conversation{}, dao.ErrNotFound
	}
	return s.activeGuest, nil
}

func (s *webConversationManagementStore) CreateAccountConversation(_ context.Context, id uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	s.createdFor = WebActor{Type: WebActorAccount, ID: id}
	return domain.Conversation{ID: 13, ProjectID: &id, ConversationNo: 3, Title: title, CurrentContext: current}, nil
}

func (s *webConversationManagementStore) CreateGuestConversation(_ context.Context, id uint64, title string, current json.RawMessage) (domain.Conversation, error) {
	s.createdFor = WebActor{Type: WebActorGuest, ID: id}
	return domain.Conversation{ID: 23, ProjectID: &id, ConversationNo: 3, Title: title, CurrentContext: current}, nil
}

func (s *webConversationManagementStore) ActivateAccountConversation(_ context.Context, accountID, conversationID uint64) (domain.Conversation, error) {
	for _, conversation := range s.accountConversations {
		if conversation.ID == conversationID {
			s.activatedFor = WebActor{Type: WebActorAccount, ID: accountID}
			s.activatedID = conversationID
			return conversation, nil
		}
	}
	return domain.Conversation{}, dao.ErrNotFound
}

func (s *webConversationManagementStore) ActivateGuestConversation(_ context.Context, guestID, conversationID uint64) (domain.Conversation, error) {
	for _, conversation := range s.guestConversations {
		if conversation.ID == conversationID {
			s.activatedFor = WebActor{Type: WebActorGuest, ID: guestID}
			s.activatedID = conversationID
			return conversation, nil
		}
	}
	return domain.Conversation{}, dao.ErrNotFound
}

func TestWebConversationManagementSupportsAccountAndGuestIsolation(t *testing.T) {
	accountID, guestID := uint64(7), uint64(8)
	store := &webConversationManagementStore{
		accountConversations: []domain.Conversation{
			{ID: 11, ProjectID: &accountID, ConversationNo: 1, Title: "账号方向一"},
			{ID: 12, ProjectID: &accountID, ConversationNo: 2, Title: "账号方向二"},
		},
		guestConversations: []domain.Conversation{
			{ID: 21, ProjectID: &guestID, ConversationNo: 1, Title: "访客方向一"},
			{ID: 22, ProjectID: &guestID, ConversationNo: 2, Title: "访客方向二"},
		},
		activeAccount: domain.Conversation{ID: 12, ProjectID: &accountID, ConversationNo: 2, Title: "账号方向二"},
		activeGuest:   domain.Conversation{ID: 21, ProjectID: &guestID, ConversationNo: 1, Title: "访客方向一"},
	}
	app := New(store, nil, nil, nil, nil)

	for _, test := range []struct {
		name             string
		actor            WebActor
		activeID         uint64
		activateID       uint64
		activatedStoreID uint64
		crossOwnerID     uint64
		createdID        uint64
		createdNumber    uint32
	}{
		{name: "account", actor: WebActor{Type: WebActorAccount, ID: accountID}, activeID: 2, activateID: 1, activatedStoreID: 11, crossOwnerID: 21, createdID: 3, createdNumber: 3},
		{name: "guest", actor: WebActor{Type: WebActorGuest, ID: guestID}, activeID: 1, activateID: 2, activatedStoreID: 22, crossOwnerID: 11, createdID: 3, createdNumber: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			conversations, err := app.ListWebConversations(context.Background(), test.actor, 100)
			if err != nil {
				t.Fatalf("list conversations: %v", err)
			}
			if len(conversations) != 2 {
				t.Fatalf("want both owned conversations, got %+v", conversations)
			}
			activeCount := 0
			for _, conversation := range conversations {
				if conversation.Active {
					activeCount++
					if conversation.ID != test.activeID {
						t.Fatalf("wrong active conversation: %+v", conversations)
					}
				}
			}
			if activeCount != 1 {
				t.Fatalf("want exactly one active conversation, got %+v", conversations)
			}

			created, err := app.CreateWebConversation(context.Background(), test.actor, "新研究方向")
			if err != nil {
				t.Fatalf("create conversation: %v", err)
			}
			if created.ID != test.createdID || created.ConversationNo != test.createdNumber || !created.Active {
				t.Fatalf("created conversation should become active, got %+v", created)
			}
			if store.createdFor.Type != test.actor.Type || store.createdFor.ID != test.actor.ID {
				t.Fatalf("created for wrong owner: %+v", store.createdFor)
			}

			activated, err := app.ActivateWebConversation(context.Background(), test.actor, test.activateID)
			if err != nil || activated.ID != test.activateID || !activated.Active {
				t.Fatalf("activate owned conversation: value=%+v err=%v", activated, err)
			}
			if store.activatedFor.Type != test.actor.Type || store.activatedFor.ID != test.actor.ID || store.activatedID != test.activatedStoreID {
				t.Fatalf("activated for wrong owner: actor=%+v id=%d", store.activatedFor, store.activatedID)
			}

			if _, err = app.ActivateWebConversation(context.Background(), test.actor, test.crossOwnerID); !errors.Is(err, dao.ErrNotFound) {
				t.Fatalf("cross-owner activation should be hidden as not found, got %v", err)
			}
		})
	}
}

func TestListWebConversationsUsesNormalizedActorType(t *testing.T) {
	accountID := uint64(7)
	store := &webConversationManagementStore{
		accountConversations: []domain.Conversation{{ID: 11, ProjectID: &accountID, ConversationNo: 1, Title: "账号方向"}},
		guestConversations:   []domain.Conversation{{ID: 21, ProjectID: &accountID, ConversationNo: 1, Title: "访客方向"}},
		activeAccount:        domain.Conversation{ID: 11, ProjectID: &accountID, ConversationNo: 1, Title: "账号方向"},
		activeGuest:          domain.Conversation{ID: 21, ProjectID: &accountID, ConversationNo: 1, Title: "访客方向"},
	}
	app := New(store, nil, nil, nil, nil)

	conversations, err := app.ListWebConversations(context.Background(), WebActor{Type: " ACCOUNT ", ID: accountID}, 100)
	if err != nil {
		t.Fatalf("list normalized account actor: %v", err)
	}
	if len(conversations) != 1 || conversations[0].ID != 1 || !conversations[0].Active {
		t.Fatalf("normalized account actor reached wrong owner: %+v", conversations)
	}
}

func TestWebReadRetriesAmbiguousCommitBeforeMarkingReservedMessageFailed(t *testing.T) {
	store := &webTestStore{commitErr: errors.New("commit acknowledgement lost"), failCommitAttempts: 1}
	app := New(store, nil, nil, nil, nil)
	var events []WebEvent
	deliver := &webDeliverer{
		app: app, actor: WebActor{Type: WebActorAccount, ID: 7}, projectNo: 2,
		emit: func(event WebEvent) error {
			events = append(events, event)
			return nil
		},
	}
	if err := deliver.Read(context.Background(), domain.Conversation{ID: 3, ConversationNo: 4}, 9, domain.ProjectReadTypeDeep, "result", 0.2); err != nil {
		t.Fatalf("ambiguous commit retry did not recover: %v", err)
	}
	if store.commitAttempts != 2 || len(store.failedMessageIDs) != 0 || !deliver.readCommitted {
		t.Fatalf("unexpected retry state: attempts=%d failed=%v committed=%v", store.commitAttempts, store.failedMessageIDs, deliver.readCommitted)
	}
	if len(events) != 2 || events[0].Status != WebStatusCommitted || events[1].Status != WebStatusDelta || events[1].Content != "result" {
		t.Fatalf("unexpected SSE sequence after recovered commit: %+v", events)
	}
}
