package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"paperassistant/internal/config"
)

type Client struct {
	cfg    config.QQConfig
	http   *http.Client
	tokens *tokenSource
}

type IncomingMessage struct {
	EventID             string
	MessageID           string
	ReferencedMessageID string
	OpenID              string
	Content             string
	Timestamp           time.Time
}

type SendReceipt struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Remaining int    `json:"remain_msg_len"`
	ExtInfo   struct {
		ReferenceIndex string `json:"ref_idx"`
	} `json:"ext_info"`
	ErrorCode    int    `json:"err_code"`
	ErrorMessage string `json:"message"`
	TraceID      string `json:"trace_id"`
}

type StreamChunk struct {
	MessageID  string
	MessageSeq uint32
	StreamID   string
	Index      int
	Content    string
	Finished   bool
}

func New(cfg config.QQConfig) *Client {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return &Client{cfg: cfg, http: httpClient, tokens: &tokenSource{appID: cfg.AppID, secret: cfg.AppSecret, tokenURL: cfg.TokenURL, http: httpClient}}
}

func (c *Client) SendText(ctx context.Context, openID, messageID string, seq uint32, content string) (SendReceipt, error) {
	payload := map[string]any{"msg_type": 0, "content": content, "msg_id": messageID, "msg_seq": seq}
	return c.post(ctx, "/v2/users/"+url.PathEscape(openID)+"/messages", payload)
}

func (c *Client) SendStream(ctx context.Context, openID string, chunk StreamChunk) (SendReceipt, error) {
	state := 1
	if chunk.Finished {
		state = 10
	}
	payload := map[string]any{
		"input_mode": "replace", "input_state": state, "index": chunk.Index,
		"content_type": "markdown", "content_raw": chunk.Content,
		"msg_id": chunk.MessageID, "msg_seq": chunk.MessageSeq,
	}
	if chunk.StreamID != "" {
		payload["stream_msg_id"] = chunk.StreamID
	}
	return c.post(ctx, "/v2/users/"+url.PathEscape(openID)+"/stream_messages", payload)
}

func (c *Client) post(ctx context.Context, path string, payload any) (SendReceipt, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return SendReceipt{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return SendReceipt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIBase+path, bytes.NewReader(body))
	if err != nil {
		return SendReceipt{}, err
	}
	req.Header.Set("Authorization", "QQBot "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		return SendReceipt{}, fmt.Errorf("QQ send: %w", err)
	}
	defer resp.Body.Close()
	var receipt SendReceipt
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return receipt, err
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &receipt)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Same reasoning as the gateway path: a rejected token must not be reused.
		c.tokens.invalidate(token)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || receipt.ErrorCode != 0 {
		return receipt, fmt.Errorf("QQ send status=%d err=%d message=%s trace=%s", resp.StatusCode, receipt.ErrorCode, strings.TrimSpace(receipt.ErrorMessage), receipt.TraceID)
	}
	return receipt, nil
}
