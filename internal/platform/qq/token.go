package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tokenSource struct {
	appID, secret, tokenURL string
	http                    *http.Client
	mu                      sync.Mutex
	token                   string
	expiresAt               time.Time
}

func (s *tokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Until(s.expiresAt) > time.Minute {
		return s.token, nil
	}
	body, _ := json.Marshal(map[string]string{"appId": s.appID, "clientSecret": s.secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("QQ access token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("QQ access token status %d", resp.StatusCode)
	}
	var result struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode QQ access token: %w", err)
	}
	if result.AccessToken == "" {
		return "", errors.New("QQ access token response is empty")
	}
	seconds := int64(7200)
	if len(result.ExpiresIn) > 0 {
		value := strings.Trim(string(result.ExpiresIn), `"`)
		if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed > 0 {
			seconds = parsed
		}
	}
	s.token = result.AccessToken
	s.expiresAt = time.Now().Add(time.Duration(seconds) * time.Second)
	return s.token, nil
}

// invalidate drops the cached token after the platform has rejected it. QQ can
// revoke a token before its stated expiry (for example when a newer one is
// issued for the same app), and without this the rejected token keeps being
// replayed until natural expiry, turning a momentary rejection into an outage
// that lasts up to the full token lifetime. The stale value is compared so a
// slow caller cannot discard a token another goroutine already refreshed.
func (s *tokenSource) invalidate(stale string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stale != "" && s.token == stale {
		s.token = ""
		s.expiresAt = time.Time{}
	}
}
