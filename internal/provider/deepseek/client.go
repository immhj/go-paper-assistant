package deepseek

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"paperassistant/internal/config"
	"paperassistant/internal/domain"
)

type ProviderError struct {
	StatusCode int
	Body       string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("deepseek status %d: %s", e.StatusCode, e.Body)
}

func IsBusy(err error) bool {
	var providerErr *ProviderError
	return errors.As(err, &providerErr) && (providerErr.StatusCode == http.StatusTooManyRequests || providerErr.StatusCode == http.StatusServiceUnavailable)
}

func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}

type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	prices  pricing

	chatMaxTokens, thinkingMaxTokens int
}

type pricing struct {
	input, cacheHit, output float64
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages    []Message
	JSON        bool
	Thinking    bool
	Temperature float64
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolDefinition struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolRequest struct {
	Messages []Message
	Tools    []ToolDefinition
	ToolName string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func New(cfg config.DeepSeekConfig) *Client {
	return &Client{
		baseURL: cfg.APIBase, apiKey: cfg.APIKey, model: cfg.Model,
		http:          &http.Client{Timeout: cfg.Timeout},
		prices:        pricing{input: cfg.PeakInputPerM, cacheHit: cfg.PeakCacheHitPerM, output: cfg.PeakOutputPerM},
		chatMaxTokens: cfg.MaxTokens, thinkingMaxTokens: cfg.ThinkingMaxTokens,
	}
}

func (c *Client) Available() bool { return c.apiKey != "" }

func (c *Client) Complete(ctx context.Context, request Request) (string, domain.ModelUsage, error) {
	if c.apiKey == "" {
		return "", domain.ModelUsage{}, errors.New("DEEPSEEK_API_KEY is not configured")
	}
	body, err := json.Marshal(c.payload(request, false))
	if err != nil {
		return "", domain.ModelUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", domain.ModelUsage{}, err
	}
	c.headers(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", domain.ModelUsage{}, fmt.Errorf("deepseek request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", domain.ModelUsage{}, responseError(resp)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens         int `json:"prompt_tokens"`
			CompletionTokens     int `json:"completion_tokens"`
			PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", domain.ModelUsage{}, fmt.Errorf("deepseek decode: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", domain.ModelUsage{}, errors.New("deepseek returned no choices")
	}
	usage := domain.ModelUsage{PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, CachedTokens: result.Usage.PromptCacheHitTokens}
	usage.CostUSD = c.cost(usage)
	warnIfTruncated("complete", result.Choices[0].FinishReason, usage)
	return result.Choices[0].Message.Content, usage, nil
}

func (c *Client) CallTool(ctx context.Context, request ToolRequest) (ToolCall, domain.ModelUsage, error) {
	if c.apiKey == "" {
		return ToolCall{}, domain.ModelUsage{}, errors.New("DEEPSEEK_API_KEY is not configured")
	}
	if len(request.Tools) == 0 {
		return ToolCall{}, domain.ModelUsage{}, errors.New("tool definitions are required")
	}
	allowed := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		if tool.Type != "function" || tool.Function.Name == "" {
			return ToolCall{}, domain.ModelUsage{}, errors.New("tool definition is incomplete")
		}
		if _, duplicate := allowed[tool.Function.Name]; duplicate {
			return ToolCall{}, domain.ModelUsage{}, fmt.Errorf("duplicate tool definition %q", tool.Function.Name)
		}
		allowed[tool.Function.Name] = struct{}{}
	}
	var toolChoice any = "required"
	if request.ToolName != "" {
		if _, ok := allowed[request.ToolName]; !ok {
			return ToolCall{}, domain.ModelUsage{}, fmt.Errorf("forced tool %q is not authorized", request.ToolName)
		}
		toolChoice = map[string]any{"type": "function", "function": map[string]string{"name": request.ToolName}}
	}
	payload := map[string]any{
		"model": c.model, "messages": request.Messages, "stream": false, "temperature": 0,
		"thinking": map[string]string{"type": "disabled"}, "tools": request.Tools,
		"tool_choice": toolChoice, "max_tokens": c.maxTokens(false),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ToolCall{}, domain.ModelUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ToolCall{}, domain.ModelUsage{}, err
	}
	c.headers(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return ToolCall{}, domain.ModelUsage{}, fmt.Errorf("deepseek tool request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolCall{}, domain.ModelUsage{}, responseError(resp)
	}
	var result struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens         int `json:"prompt_tokens"`
			CompletionTokens     int `json:"completion_tokens"`
			PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ToolCall{}, domain.ModelUsage{}, fmt.Errorf("decode deepseek tool response: %w", err)
	}
	usage := domain.ModelUsage{PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, CachedTokens: result.Usage.PromptCacheHitTokens}
	usage.CostUSD = c.cost(usage)
	if len(result.Choices) != 1 || len(result.Choices[0].Message.ToolCalls) != 1 {
		return ToolCall{}, usage, fmt.Errorf("deepseek returned %d choices and an invalid tool call count", len(result.Choices))
	}
	wire := result.Choices[0].Message.ToolCalls[0]
	if wire.Type != "function" || wire.ID == "" || wire.Function.Name == "" || wire.Function.Arguments == "" {
		return ToolCall{}, usage, errors.New("deepseek returned an incomplete tool call")
	}
	if _, ok := allowed[wire.Function.Name]; !ok {
		return ToolCall{}, usage, fmt.Errorf("deepseek returned unauthorized tool %q", wire.Function.Name)
	}
	if request.ToolName != "" && wire.Function.Name != request.ToolName {
		return ToolCall{}, usage, fmt.Errorf("deepseek returned %q instead of forced tool %q", wire.Function.Name, request.ToolName)
	}
	return ToolCall{ID: wire.ID, Name: wire.Function.Name, Arguments: wire.Function.Arguments}, usage, nil
}

func (c *Client) Stream(ctx context.Context, request Request, onDelta func(string) error) (domain.ModelUsage, error) {
	if c.apiKey == "" {
		return domain.ModelUsage{}, errors.New("DEEPSEEK_API_KEY is not configured")
	}
	body, err := json.Marshal(c.payload(request, true))
	if err != nil {
		return domain.ModelUsage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return domain.ModelUsage{}, err
	}
	c.headers(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.ModelUsage{}, fmt.Errorf("deepseek stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.ModelUsage{}, responseError(resp)
	}
	var usage domain.ModelUsage
	gotContent, done, finishReason := false, false, ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens         int `json:"prompt_tokens"`
				CompletionTokens     int `json:"completion_tokens"`
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
		}
		if err = json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, fmt.Errorf("deepseek stream malformed event: %w", err)
		}
		if chunk.Error != nil {
			return usage, fmt.Errorf("deepseek stream error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			if choice.Delta.Content != "" {
				gotContent = true
				if err = onDelta(choice.Delta.Content); err != nil {
					usage.CostUSD = c.cost(usage)
					return usage, err
				}
			}
		}
		if chunk.Usage != nil {
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.CompletionTokens = chunk.Usage.CompletionTokens
			usage.CachedTokens = chunk.Usage.PromptCacheHitTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, err
	}
	if !done {
		return usage, errors.New("deepseek stream ended without [DONE]")
	}
	if !gotContent {
		return usage, errors.New("deepseek stream returned no content")
	}
	usage.CostUSD = c.cost(usage)
	warnIfTruncated("stream", finishReason, usage)
	return usage, nil
}

func (c *Client) cost(usage domain.ModelUsage) float64 {
	factor := 0.5
	now := time.Now().UTC()
	if now.Weekday() >= time.Monday && now.Weekday() <= time.Friday {
		hour := now.Hour()
		if (hour >= 1 && hour < 4) || (hour >= 6 && hour < 10) {
			factor = 1
		}
	}
	miss := usage.PromptTokens - usage.CachedTokens
	if miss < 0 {
		miss = 0
	}
	return factor * (float64(miss)*c.prices.input + float64(usage.CachedTokens)*c.prices.cacheHit + float64(usage.CompletionTokens)*c.prices.output) / 1_000_000
}

// maxTokens caps one call's output (reasoning included for thinking calls) so a single runaway
// generation has a known worst-case cost.
func (c *Client) maxTokens(thinking bool) int {
	if thinking {
		return c.thinkingMaxTokens
	}
	return c.chatMaxTokens
}

func warnIfTruncated(kind, finishReason string, usage domain.ModelUsage) {
	if finishReason == "length" {
		slog.Warn("deepseek output hit max_tokens and was truncated", "call", kind, "completion_tokens", usage.CompletionTokens)
	}
}

func (c *Client) payload(request Request, stream bool) map[string]any {
	payload := map[string]any{
		"model": c.model, "messages": request.Messages, "stream": stream, "temperature": request.Temperature,
		"max_tokens": c.maxTokens(request.Thinking),
	}
	if request.JSON {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	thinkingType := "disabled"
	if request.Thinking {
		thinkingType = "enabled"
	}
	payload["thinking"] = map[string]string{"type": thinkingType}
	if stream {
		payload["stream_options"] = map[string]bool{"include_usage": true}
	}
	return payload
}

func (c *Client) headers(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	return &ProviderError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body))}
}
