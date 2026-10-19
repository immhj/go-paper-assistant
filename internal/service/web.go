package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
	guardprovider "paperassistant/internal/provider/guard"
)

type WebActor struct {
	Type string
	ID   uint64
	IP   string
}

const (
	WebActorAccount = "account"
	WebActorGuest   = "guest"

	WebStatusStarted   = "started"
	WebStatusProgress  = "progress"
	WebStatusMessage   = "message"
	WebStatusResult    = "result"
	WebStatusDelta     = "delta"
	WebStatusCommitted = "committed"
	WebStatusCompleted = "completed"
	WebStatusError     = "error"
)

type WebEvent struct {
	Status         string              `json:"status"`
	Code           string              `json:"code,omitempty"`
	Content        string              `json:"content,omitempty"`
	NewMessage     bool                `json:"newMessage,omitempty"`
	Papers         []domain.ShownPaper `json:"papers,omitempty"`
	Error          string              `json:"error,omitempty"`
	ProjectNo      uint32              `json:"projectNo,omitempty"`
	ConversationNo uint32              `json:"conversationNo,omitempty"`
	PaperID        uint64              `json:"paperID,omitempty"`
	ReadType       string              `json:"readType,omitempty"`
	ElapsedMS      int64               `json:"elapsedMs,omitempty"`
}

// WebConversation is the public Web-channel projection. Ownership and model context stay
// internal; callers only receive fields needed to render and select a conversation.
type WebConversation struct {
	// ID is the stable conversation number within the active project. The compatibility Web API
	// deliberately exposes this user-visible identifier rather than the database primary key.
	ID             uint64 `json:"id"`
	ConversationNo uint32 `json:"conversationNo"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Selected       bool   `json:"selected"`
	// Active is retained for the current-project compatibility facade.
	Active    bool      `json:"active,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type webDeliverer struct {
	app                *Application
	actor              WebActor
	projectNo          uint32
	emit               func(WebEvent) error
	startedAt          time.Time
	stages             []domain.TaskStage
	stageStartedAt     time.Time
	guestReservationID *string
	readPersisted      bool
	readCommitted      bool
	// stepLabel prefixes tool progress while a multi-step plan runs, e.g. "步骤 2/3：".
	stepLabel string
}

func (d *webDeliverer) beginStep(index, total int) {
	d.stepLabel = fmt.Sprintf("步骤 %d/%d：", index, total)
	// A guest read reservation and its commit belong to one step; a later step's read must
	// not inherit an earlier step's committed flag and skip releasing its own reservation.
	d.guestReservationID, d.readCommitted = nil, false
}

// searchRendering picks the Web rendering, which shows every candidate as a card.
func (d *webDeliverer) searchRendering(reply searchReply) searchChannelReply { return reply.Web }

// reportsLiveProgress is true only while a turn streams progress events to the browser.
func (d *webDeliverer) reportsLiveProgress() bool {
	return d.emit != nil && !d.startedAt.IsZero()
}

func (d *webDeliverer) reportProgress(code, text string) error {
	return d.progress(code, text)
}

// webText drops the leading "[会话 #N · 标题]" line that tools write for QQ: the Web page
// already shows the current conversation, so repeating it in every bubble is noise.
func webText(conversation domain.Conversation, text string) string {
	header := conversationHeader(conversation)
	if !strings.HasPrefix(text, header) {
		return text
	}
	return strings.TrimLeft(strings.TrimPrefix(text, header), "\n")
}

func (d *webDeliverer) elapsedMS() int64 {
	if d.startedAt.IsZero() {
		return 0
	}
	return time.Since(d.startedAt).Milliseconds()
}

func (d *webDeliverer) start(code, content string) error {
	d.stageStartedAt = time.Now()
	d.stages = append(d.stages, domain.TaskStage{Code: code})
	return d.emit(WebEvent{Status: WebStatusStarted, Code: code, Content: content, ElapsedMS: d.elapsedMS()})
}

func (d *webDeliverer) progress(code, content string) error {
	d.finishStage()
	d.stageStartedAt = time.Now()
	d.stages = append(d.stages, domain.TaskStage{Code: code})
	return d.emit(WebEvent{Status: WebStatusProgress, Code: code, Content: content, ElapsedMS: d.elapsedMS()})
}

func (d *webDeliverer) finishStage() {
	if len(d.stages) == 0 || d.stageStartedAt.IsZero() {
		return
	}
	d.stages[len(d.stages)-1].DurationMS = time.Since(d.stageStartedAt).Milliseconds()
	d.stageStartedAt = time.Time{}
}

func (d *webDeliverer) completeTrace(trace *domain.AgentTrace) {
	d.finishStage()
	trace.DurationMS = d.elapsedMS()
	trace.Stages = append([]domain.TaskStage(nil), d.stages...)
}

func (d *webDeliverer) completed() error {
	return d.emit(WebEvent{Status: WebStatusCompleted, ElapsedMS: d.elapsedMS()})
}

func (d *webDeliverer) insertMessage(ctx context.Context, conversation domain.Conversation, message domain.Message) (domain.Message, error) {
	if d.actor.ID == 0 || d.projectNo == 0 {
		return d.app.store.InsertMessage(ctx, message)
	}
	owner, err := projectOwner(d.actor)
	if err != nil {
		return domain.Message{}, err
	}
	return d.app.store.InsertProjectMessage(ctx, owner, d.projectNo, conversation.ConversationNo, message)
}

func (d *webDeliverer) Text(ctx context.Context, conversation domain.Conversation, text string, shown []domain.ShownPaper, cost float64) error {
	text = webText(conversation, text)
	if err := d.persist(ctx, conversation, text, shown, cost); err != nil {
		return err
	}
	return d.emitOutput(text, shown)
}

func (d *webDeliverer) Stream(ctx context.Context, conversation domain.Conversation, text string, cost float64) error {
	text = webText(conversation, text)
	if err := d.persist(ctx, conversation, text, nil, cost); err != nil {
		return err
	}
	return d.emitOutput(text, nil)
}

const webOutputDeltaRunes = 160

func (d *webDeliverer) emitOutput(text string, shown []domain.ShownPaper) error {
	runes := []rune(text)
	if len(runes) == 0 {
		return d.emit(WebEvent{Status: WebStatusDelta, NewMessage: true, Papers: shown})
	}
	for start := 0; start < len(runes); start += webOutputDeltaRunes {
		end := min(start+webOutputDeltaRunes, len(runes))
		event := WebEvent{Status: WebStatusDelta, Content: string(runes[start:end]), NewMessage: start == 0}
		if end == len(runes) {
			event.Papers = shown
		}
		if err := d.emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (d *webDeliverer) ModelStream(
	ctx context.Context,
	conversation domain.Conversation,
	paperID uint64,
	readType, prefix, suffix string,
	stream func(func(string) error) (domain.ModelUsage, error),
) (domain.ModelUsage, error) {
	read := paperID != 0 || readType != ""
	if read && (paperID == 0 || (readType != domain.ProjectReadTypeQuick && readType != domain.ProjectReadTypeDeep)) {
		return domain.ModelUsage{}, errors.New("invalid Web project read identity")
	}
	limiter := newModelDeltaLimiter(readOutputRunes)
	prefix = webText(conversation, prefix)
	content := prefix
	reserved, err := d.insertMessage(ctx, conversation, domain.Message{
		ConversationID: conversation.ID,
		Role:           "bot",
		Content:        &content,
		DeliveryStatus: "reserved",
	})
	if err != nil {
		return domain.ModelUsage{}, err
	}

	markFailed := func(cause error, usage domain.ModelUsage) error {
		persistCtx := context.WithoutCancel(ctx)
		partial := prefix + limiter.String()
		updateErr := d.app.store.UpdateMessageContent(persistCtx, reserved.ID, partial, usage.CostUSD)
		deliveryErr := d.app.store.FailReservedMessage(persistCtx, reserved.ID)
		return errors.Join(cause, updateErr, deliveryErr)
	}
	newMessage := true
	emitDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := d.emit(WebEvent{Status: WebStatusDelta, Content: delta, NewMessage: newMessage}); err != nil {
			return err
		}
		newMessage = false
		return nil
	}
	if err = emitDelta(prefix); err != nil {
		return domain.ModelUsage{}, markFailed(err, domain.ModelUsage{})
	}

	usage, streamErr := stream(func(delta string) error {
		return emitDelta(limiter.Append(delta))
	})
	if streamErr != nil {
		return usage, markFailed(streamErr, usage)
	}
	if strings.TrimSpace(limiter.String()) == "" {
		emptyErr := errors.New("model stream returned empty content")
		return usage, markFailed(emptyErr, usage)
	}

	content = prefix + limiter.String() + suffix
	persistCtx := context.WithoutCancel(ctx)
	if read {
		if err = d.commitRead(persistCtx, conversation, reserved.ID, paperID, readType, content, usage.CostUSD); err != nil {
			return usage, err
		}
		if err = d.emitReadCommitted(conversation, paperID, readType); err != nil {
			return usage, err
		}
	} else {
		if err = d.app.store.UpdateMessageContent(persistCtx, reserved.ID, content, usage.CostUSD); err != nil {
			return usage, markFailed(err, usage)
		}
		if err = d.app.store.UpdateMessageDelivery(persistCtx, reserved.ID, "sent", ""); err != nil {
			return usage, errors.Join(err, d.app.store.FailReservedMessage(persistCtx, reserved.ID))
		}
	}
	if err = emitDelta(suffix); err != nil {
		return usage, err
	}
	return usage, nil
}

func (d *webDeliverer) commitRead(ctx context.Context, conversation domain.Conversation, messageID, paperID uint64, readType, content string, cost float64) error {
	owner, err := projectOwner(d.actor)
	if err != nil {
		return err
	}
	commitValue := domain.ProjectReadCommit{
		ProjectOwner: owner, ProjectNo: d.projectNo, ConversationNo: conversation.ConversationNo,
		PaperID: paperID, MessageID: messageID, ReadType: readType, Content: content, CostUSD: cost,
		GuestReservationID: d.guestReservationID,
	}
	var commitErr error
	for attempt := 0; attempt < 2; attempt++ {
		if _, commitErr = d.app.store.CommitProjectReadResult(ctx, commitValue); commitErr == nil {
			d.readPersisted = true
			d.readCommitted = true
			return nil
		}
	}
	return errors.Join(commitErr, d.app.store.FailReservedMessage(ctx, messageID))
}

func (d *webDeliverer) emitReadCommitted(conversation domain.Conversation, paperID uint64, readType string) error {
	return d.emit(WebEvent{
		Status: WebStatusCommitted, ProjectNo: d.projectNo, ConversationNo: conversation.ConversationNo,
		PaperID: paperID, ReadType: readType,
	})
}

type modelDeltaLimiter struct {
	content   strings.Builder
	remaining int
	started   bool
	truncated bool
}

func newModelDeltaLimiter(limit int) *modelDeltaLimiter {
	return &modelDeltaLimiter{remaining: limit}
}

func (l *modelDeltaLimiter) Append(delta string) string {
	if !l.started {
		delta = strings.TrimLeftFunc(delta, unicode.IsSpace)
		if delta == "" {
			return ""
		}
		l.started = true
	}
	if l.truncated {
		return ""
	}
	runes := []rune(delta)
	if len(runes) <= l.remaining {
		l.remaining -= len(runes)
		l.content.WriteString(delta)
		return delta
	}
	chunk := string(runes[:l.remaining]) + "\n\n[达到精读结果长度上限，已停止输出]"
	l.content.WriteString(chunk)
	l.remaining = 0
	l.truncated = true
	return chunk
}

func (l *modelDeltaLimiter) String() string {
	return l.content.String()
}

func (d *webDeliverer) Read(ctx context.Context, conversation domain.Conversation, paperID uint64, readType, text string, cost float64) error {
	text = webText(conversation, text)
	initial := ""
	reserved, err := d.insertMessage(ctx, conversation, domain.Message{
		ConversationID: conversation.ID,
		Role:           "bot",
		Content:        &initial,
		DeliveryStatus: "reserved",
	})
	if err != nil {
		return err
	}
	commitCtx := context.WithoutCancel(ctx)
	if err = d.commitRead(commitCtx, conversation, reserved.ID, paperID, readType, text, cost); err != nil {
		return err
	}
	if err = d.emitReadCommitted(conversation, paperID, readType); err != nil {
		return err
	}
	return d.emitOutput(text, nil)
}

func (d *webDeliverer) SearchResult(ctx context.Context, conversation domain.Conversation, reply searchReply, cost float64) error {
	shown, currentContext, text := reply.Web.Shown, reply.Web.Context, reply.Web.Text
	shownJSON, _ := json.Marshal(shown)
	content := text
	owner, ownerErr := projectOwner(d.actor)
	if ownerErr != nil {
		return ownerErr
	}
	if err := d.app.store.SaveProjectSearchResult(ctx, owner, d.projectNo, conversation.ConversationNo, currentContext, domain.Message{
		ConversationID: conversation.ID, Role: "bot", Content: &content, ShownPapers: shownJSON, CostUSD: cost,
	}); err != nil {
		return err
	}
	return d.emitOutput(text, shown)
}

func (d *webDeliverer) persist(ctx context.Context, conversation domain.Conversation, text string, shown []domain.ShownPaper, cost float64) error {
	var shownJSON json.RawMessage
	if len(shown) > 0 {
		shownJSON, _ = json.Marshal(shown)
	}
	content := text
	_, err := d.insertMessage(ctx, conversation, domain.Message{
		ConversationID: conversation.ID, Role: "bot", Content: &content, ShownPapers: shownJSON,
		DeliveryStatus: "sent", CostUSD: cost,
	})
	return err
}

// RunWebTurn accepts WebActor for new callers. A uint64 is retained as an account-only
// compatibility shim for the existing controller; it is valid only while no WebGuard is injected.
func (a *Application) RunWebTurn(ctx context.Context, actorValue any, text string, emit func(WebEvent) error) (resultErr error) {
	startedAt := time.Now()
	actor, err := normalizeWebActor(actorValue)
	if err != nil {
		return emitWebError(emit, err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return emitWebError(emit, errors.New("消息不能为空"))
	}
	if a.emergency.Engaged() {
		return emitWebError(emit, errors.New("系统正在维护，暂时无法处理请求，请稍后再试。"))
	}

	var lease guardprovider.Lease
	if a.webGuard != nil {
		lease, err = a.webGuard.BeginTurn(ctx, actor.key(), actor.IP)
		if err != nil {
			return emitWebError(emit, err)
		}
		defer func() {
			resultErr = errors.Join(resultErr, a.webGuard.Release(context.WithoutCancel(ctx), lease))
		}()
	}

	project, conversation, err := a.ensureActiveChatContext(ctx, actor)
	if err != nil {
		return emitWebError(emit, err)
	}
	content := text
	owner, ownerErr := projectOwner(actor)
	if ownerErr != nil {
		return emitWebError(emit, ownerErr)
	}
	userMessage, err := a.store.InsertProjectMessage(ctx, owner, project.ProjectNo, conversation.ConversationNo, domain.Message{ConversationID: conversation.ID, Role: "user", Content: &content})
	if err != nil {
		return emitWebError(emit, err)
	}

	deliver := &webDeliverer{app: a, actor: actor, projectNo: project.ProjectNo, emit: emit, startedAt: startedAt}
	if err = deliver.start("understanding", "正在理解你的问题"); err != nil {
		return err
	}
	complete := func(trace *domain.AgentTrace, usage domain.ModelUsage, turnErr error) error {
		if turnErr != nil {
			trace.ErrorCode = webErrorCode(turnErr)
		}
		deliver.completeTrace(trace)
		finishErr := a.finishWebTurn(ctx, userMessage.ID, *trace, usage, turnErr)
		if turnErr != nil {
			emitErr := emit(WebEvent{Status: WebStatusError, Code: webErrorCode(turnErr), Error: webErrorMessage(turnErr), ElapsedMS: deliver.elapsedMS()})
			return errors.Join(turnErr, finishErr, emitErr)
		}
		return errors.Join(finishErr, deliver.completed())
	}

	turnID, randomErr := randomHex(8)
	if randomErr != nil {
		trace := domain.AgentTrace{Status: "failed", Error: randomErr.Error()}
		return complete(&trace, domain.ModelUsage{}, randomErr)
	}
	conversation, titleUsage := a.autoTitleWebConversation(ctx, actor, project.ProjectNo, conversation, text)
	if err = deliver.progress("routing", "正在规划任务"); err != nil {
		return err
	}
	input := qq.IncomingMessage{Content: text, MessageID: "web-" + turnID}
	turnCtx := withProjectAgentScope(withToolGate(ctx, &webToolGate{app: a, actor: actor, deliver: deliver}), owner, project.ProjectNo)
	plan, plannerUsage := a.planTurn(turnCtx, domain.User{}, conversation, input, parseCommand(text), maxPlanSteps)
	trace := domain.AgentTrace{
		Route: plan.Route, Outcome: plan.Outcome,
		RouterSource: plan.Source, RouterUsage: plannerUsage, Status: "routing",
	}
	totalUsage := titleUsage
	totalUsage.Add(plannerUsage)

	if plan.ProviderError != nil {
		return complete(&trace, totalUsage, plan.ProviderError)
	}
	if plan.Route == "respond" || plan.Route == "clarify" {
		err = deliver.Text(turnCtx, conversation, plan.Response, nil, 0)
		return complete(&trace, totalUsage, err)
	}
	// Web conversations are managed in the sidebar; a plan naming such a tool is always one step.
	if conversationManagementTools[plan.Steps[0].Tool] {
		err = deliver.Text(turnCtx, conversation, "请使用左侧“新建”按钮创建会话；切换会话请在会话列表中选择。", nil, 0)
		return complete(&trace, totalUsage, err)
	}
	if err = deliver.progress("execution", "正在准备执行任务"); err != nil {
		return err
	}

	steps, executeErr := a.executePlan(turnCtx, domain.User{}, conversation, input, plan.Steps, deliver)
	stepErrorCode(steps, executeErr)
	planTrace(&trace, steps)
	totalUsage.Add(trace.ToolUsage)
	return complete(&trace, totalUsage, executeErr)
}

func normalizeWebActor(value any) (WebActor, error) {
	var actor WebActor
	switch typed := value.(type) {
	case WebActor:
		actor = typed
	case uint64:
		actor = WebActor{Type: WebActorAccount, ID: typed}
	default:
		return WebActor{}, errors.New("invalid Web actor")
	}
	actor.Type = strings.ToLower(strings.TrimSpace(actor.Type))
	if actor.ID == 0 || (actor.Type != WebActorAccount && actor.Type != WebActorGuest) {
		return WebActor{}, errors.New("invalid Web actor")
	}
	return actor, nil
}

func (a WebActor) key() string { return fmt.Sprintf("%s:%d", a.Type, a.ID) }

func (a *Application) ListWebConversations(ctx context.Context, actor WebActor, limit int) ([]WebConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return nil, err
	}
	project, _, err := a.store.GetActiveProjectContext(ctx, owner)
	if errors.Is(err, dao.ErrNotFound) {
		return []WebConversation{}, nil
	}
	if err != nil {
		return nil, err
	}
	conversations, _, err := a.store.ListProjectConversations(ctx, owner, project.ProjectNo, domain.ConversationStatusActive, limit, nil)
	if err != nil {
		return nil, err
	}
	items := make([]WebConversation, 0, len(conversations))
	for _, conversation := range conversations {
		selected := project.ActiveConversationID != nil && *project.ActiveConversationID == conversation.ID
		items = append(items, projectWebConversation(conversation, selected))
	}
	return items, nil
}

func (a *Application) CreateWebConversation(ctx context.Context, actor WebActor, title string) (WebConversation, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return WebConversation{}, err
	}
	project, _, err := a.store.GetActiveProjectContext(ctx, owner)
	if err != nil {
		return WebConversation{}, err
	}
	if strings.TrimSpace(title) == "" {
		title = "新会话"
	}
	conversation, err := a.store.CreateProjectConversation(ctx, owner, project.ProjectNo, title, defaultConversationContext())
	return projectWebConversation(conversation, true), err
}

// ActivateWebConversation is the compatibility facade and interprets its argument as the stable
// conversation number inside the current project, never as a database ID.
func (a *Application) ActivateWebConversation(ctx context.Context, actor WebActor, conversationNumber uint64) (WebConversation, error) {
	if conversationNumber == 0 || conversationNumber > uint64(^uint32(0)) {
		return WebConversation{}, ErrValidation
	}
	owner, err := projectOwner(actor)
	if err != nil {
		return WebConversation{}, err
	}
	project, _, err := a.store.GetActiveProjectContext(ctx, owner)
	if err != nil {
		return WebConversation{}, err
	}
	_, conversation, err := a.store.SelectProjectConversation(ctx, owner, project.ProjectNo, uint32(conversationNumber))
	return projectWebConversation(conversation, true), err
}

func projectWebConversation(conversation domain.Conversation, active bool) WebConversation {
	return WebConversation{ID: uint64(conversation.ConversationNo), ConversationNo: conversation.ConversationNo, Title: conversation.Title, Status: conversation.Status, Selected: active, Active: active, CreatedAt: conversation.CreatedAt, UpdatedAt: conversation.UpdatedAt}
}

func (a *Application) WebHistory(ctx context.Context, actor WebActor, limit int) ([]domain.Message, error) {
	owner, err := projectOwner(actor)
	if err != nil {
		return nil, err
	}
	messages, err := a.store.ListActiveProjectMessages(ctx, owner, limit)
	if errors.Is(err, dao.ErrNotFound) {
		return []domain.Message{}, nil
	}
	return messages, err
}

func (a *Application) finishWebTurn(ctx context.Context, messageID uint64, trace domain.AgentTrace, usage domain.ModelUsage, turnErr error) error {
	trace.Status = "succeeded"
	if turnErr != nil {
		trace.Status, trace.Error = "failed", truncateRunes(turnErr.Error(), 500)
	}
	if a.webGuard != nil {
		if err := a.webGuard.RecordCost(context.WithoutCancel(ctx), usage.CostUSD); err != nil {
			turnErr = errors.Join(turnErr, err)
			trace.Status, trace.Error = "failed", truncateRunes(turnErr.Error(), 500)
		}
	}
	if err := a.store.UpdateMessageAgentTraceByID(context.WithoutCancel(ctx), messageID, agentTraceJSON(trace), usage.CostUSD); err != nil {
		turnErr = errors.Join(turnErr, err)
	}
	return turnErr
}

func emitWebError(emit func(WebEvent) error, cause error) error {
	return errors.Join(cause, emit(WebEvent{Status: WebStatusError, Code: webErrorCode(cause), Error: webErrorMessage(cause)}))
}

func webErrorCode(err error) string {
	switch {
	case deepseek.IsBusy(err):
		return "MODEL_BUSY"
	case deepseek.IsTimeout(err):
		return "MODEL_TIMEOUT"
	case errors.Is(err, dao.ErrGuestTaskInProgress):
		return "GUEST_DEEP_READ_IN_PROGRESS"
	case errors.Is(err, dao.ErrGuestDeepReadExhausted):
		return "GUEST_DEEP_READ_EXHAUSTED"
	case errors.Is(err, guardprovider.ErrRateLimited):
		return "RATE_LIMITED"
	case errors.Is(err, guardprovider.ErrConcurrent):
		return "REQUEST_IN_PROGRESS"
	case errors.Is(err, guardprovider.ErrCircuitOpen):
		return "COST_BREAKER_OPEN"
	case errors.Is(err, guardprovider.ErrUnavailable):
		return "DEPENDENCY_UNAVAILABLE"
	case strings.Contains(err.Error(), "系统正在维护"):
		return "EMERGENCY_STOPPED"
	default:
		return "INTERNAL_ERROR"
	}
}

func webErrorMessage(err error) string {
	switch {
	case deepseek.IsBusy(err):
		return "模型服务当前繁忙，请稍后重试。"
	case deepseek.IsTimeout(err):
		return "模型服务响应超时，请稍后重试。"
	case errors.Is(err, dao.ErrGuestTaskInProgress):
		return "访客精读任务正在处理中，请等待完成。"
	case errors.Is(err, dao.ErrGuestDeepReadExhausted):
		return "本访客会话的精读资格已使用。"
	case errors.Is(err, guardprovider.ErrRateLimited):
		return "请求过于频繁，请稍后再试。"
	case errors.Is(err, guardprovider.ErrConcurrent):
		return "已有请求正在处理中，请等待完成后再试。"
	case errors.Is(err, guardprovider.ErrCircuitOpen):
		return "模型服务本时段额度已用完，请稍后再试。"
	case errors.Is(err, guardprovider.ErrUnavailable):
		return "安全服务暂时不可用，请稍后再试。"
	default:
		return "处理请求时出错，请稍后重试。"
	}
}
