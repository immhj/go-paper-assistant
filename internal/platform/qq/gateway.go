package qq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var errReconnect = errors.New("QQ gateway requested reconnect")

type sessionState struct {
	SessionID string
	Sequence  atomic.Int64
}

type gatewayPayload struct {
	ID   string          `json:"id"`
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
	Seq  *int64          `json:"s"`
	Type string          `json:"t"`
}

func (c *Client) RunGateway(ctx context.Context, handler func(context.Context, IncomingMessage)) error {
	backoff := c.cfg.ReconnectMin
	var state sessionState
	for ctx.Err() == nil {
		err := c.runSession(ctx, &state, handler)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("QQ gateway disconnected", "error", err, "retry_in", backoff)
		jitter := time.Duration(rand.Int64N(int64(backoff/2 + 1))) //nolint:gosec // G404: reconnect jitter, not a security value
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > c.cfg.ReconnectMax {
			backoff = c.cfg.ReconnectMax
		}
	}
	return ctx.Err()
}

func (c *Client) runSession(ctx context.Context, state *sessionState, handler func(context.Context, IncomingMessage)) error {
	gatewayURL := c.cfg.GatewayURL
	if gatewayURL == "" {
		var err error
		gatewayURL, err = c.gatewayURL(ctx)
		if err != nil {
			return err
		}
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, gatewayURL, nil) //nolint:bodyclose // gorilla/websocket owns the handshake response body
	if err != nil {
		return fmt.Errorf("dial QQ gateway: %w", err)
	}
	defer conn.Close()

	var writeMu sync.Mutex
	write := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(value)
	}
	var hello gatewayPayload
	if err = conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("read QQ hello: %w", err)
	}
	if hello.Op != 10 {
		return fmt.Errorf("expected QQ hello op=10, got %d", hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err = json.Unmarshal(hello.Data, &helloData); err != nil || helloData.HeartbeatInterval <= 0 {
		return fmt.Errorf("invalid QQ hello heartbeat: %w", err)
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return err
	}
	authToken := "QQBot " + token
	if state.SessionID != "" {
		err = write(map[string]any{"op": 6, "d": map[string]any{"token": authToken, "session_id": state.SessionID, "seq": state.Sequence.Load()}})
	} else {
		err = write(map[string]any{"op": 2, "d": map[string]any{
			"token": authToken, "intents": c.cfg.Intents, "shard": []int{0, 1},
			"properties": map[string]string{"$os": "linux", "$browser": "paper-assistant", "$device": "paper-assistant"},
		}})
	}
	if err != nil {
		return fmt.Errorf("authenticate QQ gateway: %w", err)
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(time.Duration(helloData.HeartbeatInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				var sequence any
				sequenceValue := state.Sequence.Load()
				if sequenceValue != 0 {
					sequence = sequenceValue
				}
				if err := write(map[string]any{"op": 1, "d": sequence}); err != nil {
					slog.Error("QQ heartbeat failed", "error", err)
					_ = conn.Close()
					return
				}
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Duration(helloData.HeartbeatInterval) * time.Millisecond))
	for {
		var payload gatewayPayload
		if err = conn.ReadJSON(&payload); err != nil {
			return fmt.Errorf("read QQ gateway: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Duration(helloData.HeartbeatInterval) * time.Millisecond))
		if payload.Seq != nil {
			state.Sequence.Store(*payload.Seq)
		}
		switch payload.Op {
		case 0:
			if payload.Type == "READY" {
				var ready struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(payload.Data, &ready) == nil {
					state.SessionID = ready.SessionID
				}
				slog.Info("QQ gateway ready", "session_id", state.SessionID)
			}
			if payload.Type == "C2C_MESSAGE_CREATE" {
				message, parseErr := parseC2C(payload)
				if parseErr != nil {
					slog.Error("parse QQ C2C event", "error", parseErr)
					continue
				}
				handler(ctx, message)
			}
		case 7:
			return errReconnect
		case 9:
			state.SessionID = ""
			state.Sequence.Store(0)
			return errors.New("QQ gateway invalid session")
		}
	}
}

func (c *Client) gatewayURL(ctx context.Context) (string, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/gateway", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get QQ gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Drop the token so the next reconnect fetches a fresh one instead of
		// replaying the rejected one for the rest of its lifetime.
		c.tokens.invalidate(token)
		return "", fmt.Errorf("get QQ gateway status %d; discarded the rejected access token", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("get QQ gateway status %d", resp.StatusCode)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode QQ gateway: %w", err)
	}
	if result.URL == "" {
		return "", errors.New("QQ gateway URL is empty")
	}
	return result.URL, nil
}

func parseC2C(payload gatewayPayload) (IncomingMessage, error) {
	var data struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		Timestamp string `json:"timestamp"`
		Author    struct {
			ID         string `json:"id"`
			UserOpenID string `json:"user_openid"`
		} `json:"author"`
		MessageReference struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return IncomingMessage{}, err
	}
	openID := data.Author.UserOpenID
	if openID == "" {
		openID = data.Author.ID
	}
	if data.ID == "" || openID == "" {
		return IncomingMessage{}, errors.New("QQ C2C message has no message ID or user openid")
	}
	message := IncomingMessage{EventID: payload.ID, MessageID: data.ID, ReferencedMessageID: data.MessageReference.MessageID, OpenID: openID, Content: data.Content, Timestamp: time.Now()}
	if parsed, err := time.Parse(time.RFC3339, data.Timestamp); err == nil {
		message.Timestamp = parsed
	}
	return message, nil
}
