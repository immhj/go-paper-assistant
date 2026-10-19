// Package metrics defines the Prometheus instruments exported on the admin listener's /metrics
// (ADR 0023). Every label has a small, fixed value set: routes use the ServeMux pattern rather
// than the raw path, so IDs in URLs can never explode series cardinality.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"paperassistant/internal/domain"
)

const namespace = "paperassistant"

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "http_requests_total",
		Help: "HTTP requests by listener, route pattern, method and status code.",
	}, []string{"listener", "route", "method", "code"})

	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "http_request_duration_seconds",
		Help: "HTTP request latency by listener and route pattern. SSE chat routes include the full stream.",
		// Up to 5 minutes so long SSE turns land in a real bucket instead of +Inf.
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"listener", "route"})

	llmTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "llm_tokens_total",
		Help: "DeepSeek tokens by call kind and token type (prompt, completion, cached).",
	}, []string{"call", "type"})

	llmCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "llm_cost_usd_total",
		Help: "Estimated DeepSeek spend in USD by call kind.",
	}, []string{"call"})

	llmTruncated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "llm_truncated_total",
		Help: "DeepSeek responses cut off by max_tokens.",
	}, []string{"call"})

	rateLimited = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "rate_limited_total",
		Help: "Requests rejected by a limiter: api, login, web_turn, web_concurrent, cost_breaker.",
	}, []string{"limiter"})

	guestsCreated = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "guest_sessions_created_total",
		Help: "Guest sessions minted. A spike usually means clients dropping the cookie.",
	})
)

// Handler serves the default registry, which also carries Go runtime and process metrics.
func Handler() http.Handler { return promhttp.Handler() }

// ObserveHTTP records one finished request. route must be a ServeMux pattern (or "unmatched").
func ObserveHTTP(listener, route, method string, code int, elapsed time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	httpRequests.WithLabelValues(listener, route, method, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(listener, route).Observe(elapsed.Seconds())
}

// ObserveLLM records the usage of one DeepSeek call. call is complete, thinking, tool or stream.
func ObserveLLM(call string, usage domain.ModelUsage) {
	llmTokens.WithLabelValues(call, "prompt").Add(float64(usage.PromptTokens))
	llmTokens.WithLabelValues(call, "completion").Add(float64(usage.CompletionTokens))
	llmTokens.WithLabelValues(call, "cached").Add(float64(usage.CachedTokens))
	if usage.CostUSD > 0 {
		llmCost.WithLabelValues(call).Add(usage.CostUSD)
	}
}

func LLMTruncated(call string) { llmTruncated.WithLabelValues(call).Inc() }

func RateLimited(limiter string) { rateLimited.WithLabelValues(limiter).Inc() }

func GuestCreated() { guestsCreated.Inc() }
