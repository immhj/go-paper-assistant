//go:build liveeval

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paperassistant/internal/config"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

// This is intentionally a report, not a merge gate (ADR 0024). It uses only hand-authored
// seed fixtures; reviewed, anonymized real requests can be added without changing this runner.
type liveFixture struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Spec     domain.SearchSpec `json:"spec"`
	Texts    []string          `json:"texts"`
	Examples []struct {
		Title    string `json:"title"`
		Abstract string `json:"abstract"`
		Level    string `json:"level"`
	} `json:"examples"`
	Want struct {
		Route string   `json:"route"`
		Tools []string `json:"tools"`
	} `json:"want"`
}

type liveResult struct {
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Expected string  `json:"expected"`
	Actual   string  `json:"actual"`
	Passed   bool    `json:"passed"`
	CostUSD  float64 `json:"costUSD"`
}

type liveReport struct {
	Cases         int          `json:"cases"`
	Passed        int          `json:"passed"`
	SkippedBudget int          `json:"skippedBudget"`
	CostUSD       float64      `json:"costUSD"`
	MaxCostUSD    float64      `json:"maxCostUSD"`
	Results       []liveResult `json:"results"`
}

var errEvalBudget = errors.New("live evaluation cost budget exhausted")

// cappedEvalModel reserves a conservative worst-case charge before a request is sent. Input is
// bounded by raw byte count (an upper token bound) plus wire overhead; output is capped at 512.
// This prevents the reported $0.20 cap from being exceeded by the last dispatched request.
type cappedEvalModel struct {
	inner     ModelClient
	remaining float64
	maxTokens int
	blocked   bool
}

func newCappedEvalModel(inner ModelClient, budget float64, maxTokens int) *cappedEvalModel {
	return &cappedEvalModel{inner: inner, remaining: budget, maxTokens: maxTokens}
}
func (m *cappedEvalModel) Available() bool { return m.inner.Available() }
func (m *cappedEvalModel) Complete(ctx context.Context, request deepseek.Request) (string, domain.ModelUsage, error) {
	inputBytes := 1024
	for _, message := range request.Messages {
		inputBytes += len([]byte(message.Content))
	}
	// Peak no-cache prices match the evaluator's DeepSeek config. A byte is a conservative upper
	// bound for token count, and maxTokens bounds every non-thinking call in this evaluator.
	reserve := (float64(inputBytes)*0.30 + float64(m.maxTokens)*1.20) / 1_000_000
	if reserve > m.remaining {
		m.blocked = true
		return "", domain.ModelUsage{}, errEvalBudget
	}
	m.remaining -= reserve
	return m.inner.Complete(ctx, request)
}
func (m *cappedEvalModel) CallTool(ctx context.Context, request deepseek.ToolRequest) (deepseek.ToolCall, domain.ModelUsage, error) {
	return m.inner.CallTool(ctx, request)
}
func (m *cappedEvalModel) Stream(ctx context.Context, request deepseek.Request, delta func(string) error) (domain.ModelUsage, error) {
	return m.inner.Stream(ctx, request, delta)
}
func (m *cappedEvalModel) takeBlocked() bool { value := m.blocked; m.blocked = false; return value }

func TestLiveModelEval(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if key == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live evaluation")
	}
	fixtures := loadLiveFixtures(t)
	limit := 0.20
	if raw := os.Getenv("MODEL_EVAL_MAX_COST_USD"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%f", &limit); err != nil || limit <= 0 {
			t.Fatalf("invalid MODEL_EVAL_MAX_COST_USD %q", raw)
		}
	}
	client := deepseek.New(config.DeepSeekConfig{
		APIBase: envOr("DEEPSEEK_API_BASE", "https://api.deepseek.com"), APIKey: key,
		Model: envOr("DEEPSEEK_MODEL", "deepseek-flash"), Timeout: 180 * time.Second,
		PeakInputPerM: 0.30, PeakCacheHitPerM: 0.006, PeakOutputPerM: 1.20,
		MaxTokens: 512, ThinkingMaxTokens: 1024,
	})
	model := newCappedEvalModel(client, limit, 512)
	report := liveReport{MaxCostUSD: limit}
	for _, fixture := range fixtures {
		switch fixture.Kind {
		case "planner":
			for index, text := range fixture.Texts {
				app := &Application{model: model}
				plan, usage := app.planTurn(context.Background(), domain.User{}, domain.Conversation{}, qq.IncomingMessage{Content: text}, command{}, maxPlanSteps)
				if model.takeBlocked() {
					report.SkippedBudget++
					continue
				}
				report.CostUSD += usage.CostUSD
				expected := fixture.Want.Route + ":" + strings.Join(fixture.Want.Tools, ",")
				actual := plan.Route + ":" + strings.Join(planToolNames(plan.Steps), ",")
				passed := expected == actual
				report.Results = append(report.Results, liveResult{Name: fmt.Sprintf("%s/%d", fixture.Name, index+1), Kind: fixture.Kind, Expected: expected, Actual: actual, Passed: passed, CostUSD: usage.CostUSD})
				report.Cases++
				if passed {
					report.Passed++
				}
			}
		case "relevance":
			for index, example := range fixture.Examples {
				abstract := example.Abstract
				judgments, cost := (&Application{model: model}).judgeRelevance(context.Background(), fixture.Spec, []domain.Candidate{{Paper: domain.Paper{Title: example.Title, Abstract: &abstract}}})
				if model.takeBlocked() {
					report.SkippedBudget++
					continue
				}
				report.CostUSD += cost
				actual := "unjudged"
				if item, ok := judgments[0]; ok {
					actual = item.Level
				}
				passed := actual == example.Level
				report.Results = append(report.Results, liveResult{Name: fmt.Sprintf("%s/%d", fixture.Name, index+1), Kind: fixture.Kind, Expected: example.Level, Actual: actual, Passed: passed, CostUSD: cost})
				report.Cases++
				if passed {
					report.Passed++
				}
			}
		default:
			t.Fatalf("unknown live fixture kind %q", fixture.Kind)
		}
	}
	writeLiveReport(t, report)
	t.Logf("live eval: %d/%d passed, cost $%.6f / $%.2f, skipped=%d", report.Passed, report.Cases, report.CostUSD, limit, report.SkippedBudget)
}

func loadLiveFixtures(t *testing.T) []liveFixture {
	t.Helper()
	file, err := os.Open("testdata/live_eval.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var fixtures []liveFixture
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for line := 1; scanner.Scan(); line++ {
		var item liveFixture
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			t.Fatalf("fixture line %d: %v", line, err)
		}
		fixtures = append(fixtures, item)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func writeLiveReport(t *testing.T, report liveReport) {
	t.Helper()
	directory := os.Getenv("MODEL_EVAL_REPORT_DIR")
	if directory == "" {
		directory = "."
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "model-eval-report.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var markdown strings.Builder
	fmt.Fprintf(&markdown, "## Live model evaluation\n\n**%d/%d** cases matched; cost **$%.6f / $%.2f**; skipped for budget: **%d**.\n\n", report.Passed, report.Cases, report.CostUSD, report.MaxCostUSD, report.SkippedBudget)
	markdown.WriteString("| Case | Expected | Actual | Cost |\n|---|---|---|---:|\n")
	for _, result := range report.Results {
		if !result.Passed {
			fmt.Fprintf(&markdown, "| %s | %s | %s | $%.6f |\n", result.Name, result.Expected, result.Actual, result.CostUSD)
		}
	}
	if err = os.WriteFile(filepath.Join(directory, "model-eval-summary.md"), []byte(markdown.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
