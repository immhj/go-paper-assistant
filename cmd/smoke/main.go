// Command smoke verifies the public guest journey against a running application.
// It intentionally runs without a model key: the chat assertion covers the deterministic
// model-unavailable fallback rather than spending money or depending on external paper sources.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultBaseURL = "http://127.0.0.1:8080"
const fallbackResponse = "论文助手暂时无法理解自然语言请求。你仍可使用 /new、/list、/use、/current 和 /help。"

type smokeClient struct {
	base   *url.URL
	client *http.Client
}

func main() {
	baseValue := flag.String("base-url", env("SMOKE_BASE_URL", defaultBaseURL), "public app base URL")
	connectAddr := flag.String("connect-addr", "", "optional host:port to dial while preserving the base URL Host and TLS validation")
	healthOnly := flag.Bool("health-only", false, "check only GET /healthz; does not create guests or application records")
	flag.Parse()
	base, err := url.Parse(*baseValue)
	if err != nil || base.Scheme == "" || base.Host == "" || base.Path != "" && base.Path != "/" {
		fatal("invalid -base-url / SMOKE_BASE_URL %q", *baseValue)
	}
	base.Path = ""
	jar, err := cookiejar.New(nil)
	if err != nil {
		fatal("create cookie jar: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if *connectAddr != "" {
		if _, _, splitErr := net.SplitHostPort(*connectAddr); splitErr != nil {
			fatal("invalid -connect-addr %q: %v", *connectAddr, splitErr)
		}
		dialer := &net.Dialer{}
		address := *connectAddr
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		}
	}
	value := &smokeClient{base: base, client: &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: transport}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	value.health(ctx)
	if *healthOnly {
		fmt.Printf("smoke passed: HTTPS health verified at %s\\n", base.String())
		return
	}
	value.session(ctx)
	value.emptyConversations(ctx)
	value.chatFallback(ctx)
	value.projects(ctx)
	fmt.Printf("smoke passed: guest session, no-model SSE fallback, and active project verified at %s\n", base.String())
}

func (s *smokeClient) health(ctx context.Context) {
	var body struct {
		Status string `json:"status"`
	}
	response := s.request(ctx, http.MethodGet, "/healthz", nil, "")
	defer response.Body.Close()
	requireStatus(response, http.StatusOK)
	decode(response.Body, &body)
	if body.Status != "ok" {
		fatal("GET /healthz: status=%q, want ok", body.Status)
	}
}

func (s *smokeClient) session(ctx context.Context) {
	var body struct {
		ActorType string `json:"actorType"`
		Guest     *struct {
			ExpiresAt     string `json:"expiresAt"`
			DeepReadState string `json:"deepReadState"`
		} `json:"guest"`
	}
	response := s.request(ctx, http.MethodGet, "/api/session", nil, "")
	defer response.Body.Close()
	requireStatus(response, http.StatusOK)
	if cookie := findCookie(response.Cookies(), "guest_session"); cookie == nil || !cookie.HttpOnly || cookie.Path != "/" || cookie.MaxAge <= 0 {
		fatal("GET /api/session: guest_session cookie is missing or unsafe")
	}
	decode(response.Body, &body)
	if body.ActorType != "guest" || body.Guest == nil || body.Guest.ExpiresAt == "" || body.Guest.DeepReadState == "" {
		fatal("GET /api/session: unexpected guest payload")
	}
}

func (s *smokeClient) emptyConversations(ctx context.Context) {
	var body []json.RawMessage
	response := s.request(ctx, http.MethodGet, "/api/conversations", nil, "")
	defer response.Body.Close()
	requireStatus(response, http.StatusOK)
	decode(response.Body, &body)
	if len(body) != 0 {
		fatal("GET /api/conversations: fresh guest has %d conversations, want 0", len(body))
	}
}

func (s *smokeClient) chatFallback(ctx context.Context) {
	response := s.request(ctx, http.MethodPost, "/api/chat", map[string]string{"text": "smoke fallback"}, s.base.String())
	defer response.Body.Close()
	requireStatus(response, http.StatusOK)
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		fatal("POST /api/chat: content type %q, want text/event-stream", response.Header.Get("Content-Type"))
	}

	started, progress, delta, completed, done := false, false, "", false, false
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			break
		}
		var event struct {
			Status  string `json:"status"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			fatal("POST /api/chat: malformed SSE event %q: %v", data, err)
		}
		switch event.Status {
		case "started":
			started = true
		case "progress":
			progress = true
		case "delta":
			delta += event.Content
		case "completed":
			completed = true
		case "error":
			fatal("POST /api/chat: unexpected error event: %s", event.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		fatal("POST /api/chat: read SSE: %v", err)
	}
	if !started || !progress || !completed || !done || delta != fallbackResponse {
		fatal("POST /api/chat: unexpected SSE lifecycle started=%t progress=%t completed=%t done=%t delta=%q", started, progress, completed, done, delta)
	}
}

func (s *smokeClient) projects(ctx context.Context) {
	var body struct {
		Items []struct {
			ProjectNo            uint32  `json:"projectNo"`
			Status               string  `json:"status"`
			Selected             bool    `json:"selected"`
			ActiveConversationNo *uint32 `json:"activeConversationNo"`
		} `json:"items"`
	}
	response := s.request(ctx, http.MethodGet, "/api/projects?status=active&limit=20", nil, "")
	defer response.Body.Close()
	requireStatus(response, http.StatusOK)
	decode(response.Body, &body)
	for _, project := range body.Items {
		if project.ProjectNo > 0 && project.Status == "active" && project.Selected && project.ActiveConversationNo != nil {
			return
		}
	}
	fatal("GET /api/projects: no selected active project after chat")
}

func (s *smokeClient) request(ctx context.Context, method, path string, payload any, origin string) *http.Response {
	endpoint := s.base.ResolveReference(&url.URL{Path: path})
	if strings.Contains(path, "?") {
		parsed, err := url.Parse(path)
		if err != nil {
			fatal("parse smoke path %q: %v", path, err)
		}
		endpoint = s.base.ResolveReference(parsed)
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			fatal("marshal %s %s: %v", method, path, err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		fatal("create %s %s: %v", method, endpoint, err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := s.client.Do(request)
	if err != nil {
		fatal("%s %s: %v", method, endpoint, err)
	}
	return response
}

func requireStatus(response *http.Response, expected int) {
	if response.StatusCode == expected {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	fatal("%s %s: status=%d, want %d, body=%s", response.Request.Method, response.Request.URL, response.StatusCode, expected, strings.TrimSpace(string(body)))
}

func decode(reader io.Reader, target any) {
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		fatal("decode JSON response: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		fatal("JSON response has trailing content")
	}
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "smoke failed: "+format+"\n", args...)
	os.Exit(1)
}
