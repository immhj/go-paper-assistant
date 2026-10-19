package router_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestMetricsUseRoutePatternsAndLiveOnlyOnAdminListener(t *testing.T) {
	fixture := newTestServer(t)
	do(t, fixture.h, http.MethodGet, "/api/session", nil, nil)
	do(t, fixture.h, http.MethodGet, "/api/projects/12345/papers/67890/read-results", nil, nil)
	do(t, fixture.h, http.MethodGet, "/no/such/path/98765", nil, nil)

	if rec := do(t, fixture.h, http.MethodGet, "/metrics", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("public /metrics: got %d, want 404", rec.Code)
	}
	rec := do(t, fixture.admin, http.MethodGet, "/metrics", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin /metrics: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`paperassistant_http_requests_total{code="200",listener="public",method="GET",route="GET /api/session"}`,
		`route="unmatched"`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %s", want)
		}
	}
	// Raw IDs must never become label values.
	for _, leak := range []string{"12345", "98765"} {
		if strings.Contains(body, leak) {
			t.Fatalf("raw path segment %q leaked into metric labels", leak)
		}
	}
}
