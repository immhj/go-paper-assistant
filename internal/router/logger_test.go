package router

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestAccessLogRecordsStatusIPAndRequestID(t *testing.T) {
	logs := captureLogs(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	h := logger("public", trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/thing?token=secret", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("access log is not one JSON line: %v\n%s", err, logs.String())
	}
	id := rec.Header().Get("X-Request-ID")
	if len(id) != 16 || entry["request_id"] != id {
		t.Fatalf("request id header %q vs log %v", id, entry["request_id"])
	}
	if entry["status"] != float64(http.StatusTeapot) || entry["bytes"] != float64(15) ||
		entry["client_ip"] != "203.0.113.9" || entry["level"] != "WARN" {
		t.Fatalf("unexpected access log: %v", entry)
	}
	if entry["path"] != "/api/thing" || bytes.Contains(logs.Bytes(), []byte("secret")) {
		t.Fatalf("query string leaked into the log: %s", logs.String())
	}
}

func TestAccessLogWrapperKeepsSSEFlushing(t *testing.T) {
	captureLogs(t)
	flushed := false
	h := logger("public", nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped writer is not an http.Flusher; SSE chat would fail")
		}
		_, _ = w.Write([]byte("data: x\n\n"))
		flusher.Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", nil))
	flushed = rec.Flushed
	if !flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
}

func TestPanicIsLoggedAs500WithRequestID(t *testing.T) {
	logs := captureLogs(t)
	h := logger("public", nil, recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if rec.Code != http.StatusInternalServerError || !bytes.Contains(logs.Bytes(), []byte(`"status":500`)) ||
		!bytes.Contains(logs.Bytes(), []byte(rec.Header().Get("X-Request-ID"))) {
		t.Fatalf("panic not access-logged as 500: code=%d logs=%s", rec.Code, logs.String())
	}
}
