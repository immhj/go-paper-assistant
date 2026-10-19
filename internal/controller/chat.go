package controller

import (
	"encoding/json"
	"errors"
	"net/http"

	"paperassistant/internal/auth"
	"paperassistant/internal/dao"
	"paperassistant/internal/service"
)

// Chat serves the Web paper-assistant chat channel over SSE for durable account or guest actors.
type Chat struct {
	app      *service.Application
	platform *Platform
}

func NewChat(app *service.Application, platform *service.Platform) *Chat {
	_ = platform
	return &Chat{app: app}
}

// BindPlatformCSRF attaches the controller-level same-origin policy used by compatibility
// conversation mutations.
func (c *Chat) BindPlatformCSRF(platform *Platform) *Chat { c.platform = platform; return c }

func (c *Chat) ListConversations(w http.ResponseWriter, r *http.Request) {
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "需要有效的账号或访客会话")
		return
	}
	if c.app == nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "聊天服务暂时不可用")
		return
	}
	conversations, err := c.app.ListWebConversations(r.Context(), actor, 100)
	if err != nil {
		writeChatError(w, err, "会话列表暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, conversations)
}

func (c *Chat) CreateConversation(w http.ResponseWriter, r *http.Request) {
	if c.platform == nil || !c.platform.sameOrigin(r) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return
	}
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "需要有效的账号或访客会话")
		return
	}
	var payload struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	if c.app == nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "聊天服务暂时不可用")
		return
	}
	conversation, err := c.app.CreateWebConversation(r.Context(), actor, payload.Title)
	if err != nil {
		writeChatError(w, err, "新建会话失败")
		return
	}
	writeJSON(w, http.StatusCreated, conversation)
}

func (c *Chat) ActivateConversation(w http.ResponseWriter, r *http.Request) {
	if c.platform == nil || !c.platform.sameOrigin(r) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return
	}
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "需要有效的账号或访客会话")
		return
	}
	conversationID, err := pathUint64(r, "conversationID")
	if err != nil || conversationID == 0 {
		writeStatusError(w, http.StatusBadRequest, "VALIDATION_ERROR", "会话 ID 无效")
		return
	}
	if c.app == nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "聊天服务暂时不可用")
		return
	}
	conversation, err := c.app.ActivateWebConversation(r.Context(), actor, conversationID)
	if err != nil {
		writeChatError(w, err, "切换会话失败")
		return
	}
	writeJSON(w, http.StatusOK, conversation)
}

func writeChatError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, service.ErrValidation):
		writeStatusError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求参数无效")
	case errors.Is(err, dao.ErrNotFound):
		writeStatusError(w, http.StatusNotFound, "NOT_FOUND", "会话不存在")
	case errors.Is(err, dao.ErrGuestUnavailable), errors.Is(err, service.ErrGuestUnavailable):
		writeStatusError(w, http.StatusUnauthorized, "GUEST_SESSION_INVALID", "访客会话无效")
	default:
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", fallback)
	}
}

func (c *Chat) History(w http.ResponseWriter, r *http.Request) {
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "需要有效的账号或访客会话")
		return
	}
	messages, err := c.app.WebHistory(r.Context(), actor, 100)
	if err != nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "聊天历史暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

func webActorFromContext(r *http.Request) (service.WebActor, bool) {
	actor := service.WebActor{IP: auth.ClientIPFromContext(r.Context())}
	if account, ok := auth.AccountFromContext(r.Context()); ok {
		actor.Type, actor.ID = service.WebActorAccount, account.ID
		return actor, true
	}
	if guest, ok := auth.GuestFromContext(r.Context()); ok {
		actor.Type, actor.ID = service.WebActorGuest, guest.ID
		return actor, true
	}
	return service.WebActor{}, false
}

func (c *Chat) Send(w http.ResponseWriter, r *http.Request) {
	actor, ok := webActorFromContext(r)
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "需要有效的账号或访客会话")
		return
	}
	var payload struct {
		Text string `json:"text"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	if c.app == nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "聊天服务暂时不可用")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeStatusError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "当前连接不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event service.WebEvent) error {
		encoded, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := w.Write([]byte("data: " + string(encoded) + "\n\n")); writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	}
	_ = c.app.RunWebTurn(r.Context(), actor, payload.Text, emit)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}
