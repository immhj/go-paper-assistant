package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
	guardprovider "paperassistant/internal/provider/guard"
)

type PaperSource interface {
	Search(context.Context, domain.PaperSearchRequest) ([]domain.Candidate, error)
	GetByID(context.Context, string, string) (domain.Candidate, error)
}

type ModelClient interface {
	Available() bool
	Complete(context.Context, deepseek.Request) (string, domain.ModelUsage, error)
	CallTool(context.Context, deepseek.ToolRequest) (deepseek.ToolCall, domain.ModelUsage, error)
	Stream(context.Context, deepseek.Request, func(string) error) (domain.ModelUsage, error)
}

type FullTextExtractor interface {
	Fetch(context.Context, uint64, string) (path string, content string, err error)
	// ExtractUploaded validates an uploaded PDF and returns verified text without persisting it.
	ExtractUploaded(context.Context, io.Reader) (content string, err error)
}

type Messenger interface {
	SendText(context.Context, string, string, uint32, string) (qq.SendReceipt, error)
	SendStream(context.Context, string, qq.StreamChunk) (qq.SendReceipt, error)
}

type Application struct {
	store                dao.Store
	platformStore        dao.PlatformStore
	webGuard             guardprovider.WebGuard
	guestDeepReadEnabled bool
	papers               PaperSource
	model                ModelClient
	fulltext             FullTextExtractor
	messenger            Messenger
	emergency            *EmergencyStop
	seen                 sync.Map
	workers              sync.Map
	workerWG             sync.WaitGroup
	locks                sync.Map
	toolUsage            sync.Map
	uploadLimiter        *slidingWindowLimiter
}

type ApplicationOption func(*Application)

func WithPlatformStore(store dao.PlatformStore) ApplicationOption {
	return func(app *Application) { app.platformStore = store }
}

func WithWebGuard(webGuard guardprovider.WebGuard) ApplicationOption {
	return func(app *Application) { app.webGuard = webGuard }
}

func WithGuestDeepReadEnabled(enabled bool) ApplicationOption {
	return func(app *Application) { app.guestDeepReadEnabled = enabled }
}

func New(store dao.Store, papers PaperSource, model ModelClient, fulltext FullTextExtractor, messenger Messenger, options ...ApplicationOption) *Application {
	app := &Application{
		store: store, papers: papers, model: model, fulltext: fulltext, messenger: messenger,
		emergency: NewEmergencyStop(), guestDeepReadEnabled: true,
		uploadLimiter: newSlidingWindowLimiter(privateFullTextUploadsPerHour, time.Hour, time.Now),
	}
	for _, option := range options {
		if option != nil {
			option(app)
		}
	}
	return app
}

// EmergencyStop exposes the global emergency stop so the admin surface can read and toggle
// the same in-memory state the inbound path checks.
func (a *Application) EmergencyStop() *EmergencyStop { return a.emergency }

type queuedMessage struct {
	ctx   context.Context
	input qq.IncomingMessage
}

func (a *Application) HandleQQ(ctx context.Context, input qq.IncomingMessage) {
	// The emergency stop is checked before the dedupe cache on purpose: a rejected message
	// leaves no trace at all, so resending the same QQ message after the stop is released is
	// accepted normally instead of being swallowed as a duplicate.
	if a.emergency.Engaged() {
		a.rejectForEmergencyStop(ctx, input)
		return
	}
	if _, loaded := a.seen.LoadOrStore(input.MessageID, struct{}{}); loaded {
		slog.Info("ignore duplicate QQ message", "message_id", input.MessageID)
		return
	}
	// Keep the cache bounded. Database idempotency is a later migration; this TTL
	// protects reconnect duplicates during the current process lifetime.
	time.AfterFunc(2*time.Hour, func() { a.seen.Delete(input.MessageID) })

	channel := make(chan queuedMessage, 32)
	actual, loaded := a.workers.LoadOrStore(input.OpenID, channel)
	if loaded {
		channel = actual.(chan queuedMessage)
	} else {
		a.workerWG.Add(1)
		go a.runUserWorker(ctx, input.OpenID, channel)
	}
	select {
	case channel <- queuedMessage{ctx: ctx, input: input}:
	case <-ctx.Done():
		a.seen.Delete(input.MessageID)
	}
}

// emergencyStopNotice tells the user the request was not recorded, so the only way forward is
// to resend it. Queueing rejected messages for replay is deliberately not done: it would fire
// a spike at the upstream model the moment the stop is released.
const emergencyStopNotice = "系统正在维护，暂时无法处理请求。这条消息没有被记录，维护结束后请重新发送一次。"

// rejectForEmergencyStop answers without touching the database: no user, no conversation, no
// message row and no dedupe entry.
func (a *Application) rejectForEmergencyStop(ctx context.Context, input qq.IncomingMessage) {
	slog.Warn("reject QQ message: emergency stop engaged", "message_id", input.MessageID, "openid", input.OpenID)
	if _, err := a.messenger.SendText(ctx, input.OpenID, input.MessageID, 1, emergencyStopNotice); err != nil {
		slog.Error("send emergency stop notice", "message_id", input.MessageID, "error", err)
	}
}

func (a *Application) runUserWorker(ctx context.Context, openID string, channel <-chan queuedMessage) {
	defer a.workerWG.Done()
	defer a.workers.Delete(openID)
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-channel:
			if err := a.handleQQ(item.ctx, item.input); err != nil {
				a.seen.Delete(item.input.MessageID)
				slog.Error("handle QQ message", "message_id", item.input.MessageID, "openid", item.input.OpenID, "error", err)
			}
		}
	}
}

func (a *Application) handleQQ(ctx context.Context, input qq.IncomingMessage) error {
	user, err := a.store.UpsertUser(ctx, input.OpenID)
	if err != nil {
		return err
	}
	exists, err := a.store.HasUserMessage(ctx, input.MessageID)
	if err != nil {
		return err
	}
	if exists {
		recoverable, recoverErr := a.store.FindRecoverableBotReply(ctx, input.MessageID)
		if recoverErr == nil {
			conversation, conversationErr := a.store.GetConversation(ctx, user.ID, recoverable.ConversationID)
			if conversationErr != nil {
				return conversationErr
			}
			_ = a.store.UpdateMessageDelivery(ctx, recoverable.ID, "failed", "")
			return a.replyText(ctx, conversation, input, "上一次回复在发送或提交阶段中断，请把你的请求作为一条新消息重新发送。", nil, 0)
		}
		if !errors.Is(recoverErr, dao.ErrNotFound) {
			return recoverErr
		}
		replyCount, countErr := a.store.CountBotReplies(ctx, input.MessageID)
		if countErr != nil {
			return countErr
		}
		if replyCount == 0 {
			userMessage, messageErr := a.store.GetUserMessageByQQID(ctx, input.MessageID)
			if messageErr != nil {
				return messageErr
			}
			conversation, conversationErr := a.store.GetConversation(ctx, user.ID, userMessage.ConversationID)
			if conversationErr != nil {
				return conversationErr
			}
			return a.replyText(ctx, conversation, input, "上一次处理在生成回复前中断，请把你的请求作为一条新消息重新发送。", nil, 0)
		}
		slog.Info("ignore persisted duplicate QQ message", "message_id", input.MessageID)
		return nil
	}
	parsed := parseCommand(input.Content)
	var conversation domain.Conversation
	if parsed.Name != "new" {
		conversation, err = a.ensureActiveConversation(ctx, user)
		if err != nil {
			return err
		}
		if err = a.recordUserMessage(ctx, conversation.ID, input); err != nil {
			return err
		}
	}

	// QQ plans are always one step until QQ is covered by the cost breaker (ADR 0021).
	plan, plannerUsage := a.planTurn(ctx, user, conversation, input, parsed, 1)
	trace := domain.AgentTrace{
		Route: plan.Route, Outcome: plan.Outcome,
		RouterSource: plan.Source, RouterUsage: plannerUsage, Status: "routing",
	}
	totalUsage := plannerUsage
	if plan.Route == "respond" || plan.Route == "clarify" {
		if conversation.ID == 0 {
			conversation, err = a.ensureActiveConversation(ctx, user)
			if err == nil {
				err = a.recordUserMessage(ctx, conversation.ID, input)
			}
		}
		if err == nil {
			err = a.replyText(ctx, conversation, input, plan.Response, nil, 0)
		}
		trace.Status = "succeeded"
		if err != nil {
			trace.Status, trace.Error = "failed", err.Error()
		}
		a.persistAgentTrace(ctx, input, trace, totalUsage)
		return err
	}

	deliver := qqDeliverer{app: a, input: input, userID: user.ID}
	steps, executeErr := a.executePlan(ctx, user, conversation, input, plan.Steps, deliver)
	stepErrorCode(steps, executeErr)
	planTrace(&trace, steps)
	trace.Status = "succeeded"
	if executeErr != nil {
		// Delivered tool output already carries its own cost; only a failed turn folds it in.
		totalUsage.Add(trace.ToolUsage)
		trace.Status, trace.Error = "failed", truncateRunes(executeErr.Error(), 500)
	}
	a.persistAgentTrace(ctx, input, trace, totalUsage)
	return executeErr
}

func (a *Application) ensureActiveConversation(ctx context.Context, user domain.User) (domain.Conversation, error) {
	conversation, err := a.store.GetActiveConversation(ctx, user.ID)
	if err == nil {
		return conversation, nil
	}
	if !errors.Is(err, dao.ErrNotFound) {
		return domain.Conversation{}, err
	}
	current, _ := json.Marshal(newSearchContext("默认会话"))
	return a.store.CreateConversation(ctx, user.ID, "默认会话", current)
}

func (a *Application) recordUserMessage(ctx context.Context, conversationID uint64, input qq.IncomingMessage) error {
	content, qqID := input.Content, input.MessageID
	_, err := a.store.InsertMessage(ctx, domain.Message{ConversationID: conversationID, Role: "user", QQMessageID: &qqID, Content: &content})
	if errors.Is(err, dao.ErrDuplicate) {
		return nil
	}
	return err
}

func (a *Application) replyText(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, text string, shown []domain.ShownPaper, cost float64) error {
	count, err := a.store.CountBotReplies(ctx, input.MessageID)
	if err != nil {
		return err
	}
	if count >= 4 {
		return errors.New("QQ passive reply quota exhausted")
	}
	seq := uint32(count + 1)
	content, qqID := text, input.MessageID
	var shownJSON json.RawMessage
	if shown != nil {
		shownJSON, _ = json.Marshal(shown)
	}
	message, err := a.store.InsertMessage(ctx, domain.Message{
		ConversationID: conversation.ID, Role: "bot", QQMessageID: &qqID, MessageSeq: &seq,
		Content: &content, ShownPapers: shownJSON, CostUSD: cost,
	})
	if err != nil {
		return err
	}
	receipt, err := a.messenger.SendText(ctx, input.OpenID, input.MessageID, seq, text)
	if err != nil {
		_ = a.store.UpdateMessageDelivery(ctx, message.ID, "failed", "")
		return fmt.Errorf("send reserved bot message %d: %w", message.ID, err)
	}
	if err = a.store.UpdateMessageDelivery(ctx, message.ID, "sent", receipt.ID); err != nil {
		return fmt.Errorf("confirm bot message %d delivery: %w", message.ID, err)
	}
	return nil
}

func (a *Application) sendFinalStreamWithSeq(ctx context.Context, input qq.IncomingMessage, seq uint32, final string) (string, error) {
	initial := streamPrefix(final, 180)
	first, err := a.messenger.SendStream(ctx, input.OpenID, qq.StreamChunk{
		MessageID: input.MessageID, MessageSeq: seq, Index: 0, Content: initial,
	})
	if err != nil {
		return "", fmt.Errorf("start final QQ stream: %w", err)
	}
	if _, err = a.messenger.SendStream(ctx, input.OpenID, qq.StreamChunk{
		MessageID: input.MessageID, MessageSeq: seq, StreamID: first.ID,
		Index: 1, Content: final, Finished: true,
	}); err != nil {
		return "", fmt.Errorf("finish final QQ stream: %w", err)
	}
	return first.ID, nil
}

func streamPrefix(content string, maxRunes int) string {
	runes := []rune(content)
	if maxRunes < 1 || len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes])
}

func (a *Application) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Application) lockConversation(id uint64) func() {
	value, _ := a.locks.LoadOrStore(id, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func newSearchContext(name string) domain.SearchContext {
	return domain.SearchContext{Name: name, SpecVersion: 1, NextOrdinal: 1, Ordinals: map[string]int{}}
}

func parseSearchContext(raw json.RawMessage, fallback string) domain.SearchContext {
	value := newSearchContext(fallback)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &value)
	}
	if value.Name == "" {
		value.Name = fallback
	}
	if value.SpecVersion < 1 {
		value.SpecVersion = 1
	}
	if value.NextOrdinal < 1 {
		value.NextOrdinal = 1
	}
	if value.Ordinals == nil {
		value.Ordinals = map[string]int{}
	}
	return value
}

func ordinalPaperID(current domain.SearchContext, ordinal int) (uint64, bool) {
	for paperID, number := range current.Ordinals {
		if number == ordinal {
			id, err := strconv.ParseUint(paperID, 10, 64)
			return id, err == nil
		}
	}
	return 0, false
}
