package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"paperassistant/internal/config"
)

// captureServer records the max_tokens of every request and answers with a minimal valid body.
func captureServer(t *testing.T, seen *[]float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		value, ok := payload["max_tokens"].(float64)
		if !ok {
			t.Errorf("request has no numeric max_tokens: %v", payload["max_tokens"])
		}
		*seen = append(*seen, value)
		w.Header().Set("Content-Type", "application/json")
		if _, isTool := payload["tools"]; isTool {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}}],"usage":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"completion_tokens":1}}`))
	}))
}

func TestEveryCallSendsTheConfiguredMaxTokens(t *testing.T) {
	var seen []float64
	server := captureServer(t, &seen)
	defer server.Close()
	client := New(config.DeepSeekConfig{
		APIBase: server.URL, APIKey: "test", Model: "m", Timeout: 5 * time.Second,
		MaxTokens: 4096, ThinkingMaxTokens: 16384,
	})
	ctx := context.Background()
	if _, _, err := client.Complete(ctx, Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Complete(ctx, Request{Thinking: true, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	tools := []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "t"}}}
	if _, _, err := client.CallTool(ctx, ToolRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Tools: tools}); err != nil {
		t.Fatal(err)
	}
	want := []float64{4096, 16384, 4096}
	if len(seen) != len(want) {
		t.Fatalf("saw %d requests, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("request %d max_tokens = %v, want %v", i, seen[i], want[i])
		}
	}
}
