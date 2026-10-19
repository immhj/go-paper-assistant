package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

// replayModel never owns credentials or an HTTP client. It replays one captured model response
// through the same ModelClient boundary production code uses.
type replayModel struct {
	output   string
	calls    int
	requests []deepseek.Request
}

func (m *replayModel) Available() bool { return true }
func (m *replayModel) Complete(_ context.Context, request deepseek.Request) (string, domain.ModelUsage, error) {
	m.calls++
	m.requests = append(m.requests, request)
	return m.output, domain.ModelUsage{CostUSD: 0.01}, nil
}
func (m *replayModel) CallTool(context.Context, deepseek.ToolRequest) (deepseek.ToolCall, domain.ModelUsage, error) {
	return deepseek.ToolCall{}, domain.ModelUsage{}, errors.New("replay must not call tools")
}
func (m *replayModel) Stream(context.Context, deepseek.Request, func(string) error) (domain.ModelUsage, error) {
	return domain.ModelUsage{}, errors.New("replay must not stream")
}

type replayFixture struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Text       string `json:"text"`
	MaxSteps   int    `json:"maxSteps"`
	Output     string `json:"output"`
	Candidates []struct {
		Title    string  `json:"title"`
		Abstract *string `json:"abstract"`
	} `json:"candidates"`
	Want struct {
		Route            string            `json:"route"`
		Source           string            `json:"source"`
		Tools            []string          `json:"tools"`
		ResponseContains string            `json:"responseContains"`
		Levels           map[string]string `json:"levels"`
		CostUSD          float64           `json:"costUSD"`
		Bounded          bool              `json:"bounded"`
	} `json:"want"`
}

func loadReplayFixtures(t *testing.T) []replayFixture {
	t.Helper()
	file, err := os.Open("testdata/model_replay.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fixtures := make([]replayFixture, 0)
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 16*1024), 256*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var fixture replayFixture
		if err := json.Unmarshal([]byte(text), &fixture); err != nil {
			t.Fatalf("fixture line %d: %v", line, err)
		}
		if fixture.Kind == "" || fixture.Name == "" || fixture.Output == "" {
			t.Fatalf("fixture line %d is incomplete: %+v", line, fixture)
		}
		key := fixture.Kind + "/" + fixture.Name
		if seen[key] {
			t.Fatalf("duplicate fixture name %q", key)
		}
		seen[key] = true
		fixtures = append(fixtures, fixture)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestModelReplayPlanner(t *testing.T) {
	for _, fixture := range loadReplayFixtures(t) {
		if fixture.Kind != "planner" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			model := &replayModel{output: fixture.Output}
			app := &Application{model: model}
			plan, usage := app.planTurn(context.Background(), domain.User{}, domain.Conversation{},
				qq.IncomingMessage{Content: fixture.Text}, command{}, fixture.MaxSteps)
			if plan.Route != fixture.Want.Route || plan.Source != fixture.Want.Source {
				t.Fatalf("plan route/source = %q/%q, want %q/%q; plan=%+v", plan.Route, plan.Source, fixture.Want.Route, fixture.Want.Source, plan)
			}
			if fixture.Want.ResponseContains != "" && !strings.Contains(plan.Response, fixture.Want.ResponseContains) {
				t.Fatalf("response %q does not contain %q", plan.Response, fixture.Want.ResponseContains)
			}
			tools := planToolNames(plan.Steps)
			if len(fixture.Want.Tools) > 0 && strings.Join(tools, ",") != strings.Join(fixture.Want.Tools, ",") {
				t.Fatalf("tools=%v, want %v", tools, fixture.Want.Tools)
			}
			if model.calls != 1 || len(model.requests) != 1 || !model.requests[0].JSON || len(model.requests[0].Messages) != 2 || usage.CostUSD != 0.01 {
				t.Fatalf("unexpected model contract: calls=%d requests=%+v usage=%+v", model.calls, model.requests, usage)
			}
			if !strings.Contains(model.requests[0].Messages[1].Content, fmt.Sprintf(`"maxSteps":%d`, fixture.MaxSteps)) {
				t.Fatalf("planner request did not state maxSteps=%d", fixture.MaxSteps)
			}
		})
	}
}

func TestModelReplayRelevance(t *testing.T) {
	for _, fixture := range loadReplayFixtures(t) {
		if fixture.Kind != "relevance" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			candidates := make([]domain.Candidate, 0, len(fixture.Candidates))
			for _, item := range fixture.Candidates {
				candidates = append(candidates, domain.Candidate{Paper: domain.Paper{Title: item.Title, Abstract: item.Abstract}})
			}
			model := &replayModel{output: fixture.Output}
			judgments, cost := (&Application{model: model}).judgeRelevance(context.Background(), domain.SearchSpec{}, candidates)
			if cost != fixture.Want.CostUSD || model.calls != 1 || len(model.requests) != 1 || !model.requests[0].JSON {
				t.Fatalf("unexpected relevance model contract: cost=%v calls=%d request=%+v", cost, model.calls, model.requests)
			}
			if len(judgments) != len(fixture.Want.Levels) {
				t.Fatalf("judgment count=%d, want %d: %+v", len(judgments), len(fixture.Want.Levels), judgments)
			}
			for rawIndex, level := range fixture.Want.Levels {
				index, err := strconv.Atoi(rawIndex)
				if err != nil {
					t.Fatalf("invalid expected index %q: %v", rawIndex, err)
				}
				judgment, ok := judgments[index]
				if !ok || judgment.Level != level {
					t.Fatalf("judgment[%d]=%+v (present=%t), want level %q", index, judgment, ok, level)
				}
				if fixture.Want.Bounded {
					if judgment.Reason != strings.TrimSpace(judgment.Reason) || judgment.SummaryZH != strings.TrimSpace(judgment.SummaryZH) {
						t.Fatalf("judgment[%d] was not trimmed: %+v", index, judgment)
					}
					if len([]rune(judgment.Reason)) > 61 || len([]rune(judgment.SummaryZH)) > 121 {
						t.Fatalf("judgment[%d] exceeded bounded output: reason=%d summary=%d", index, len([]rune(judgment.Reason)), len([]rune(judgment.SummaryZH)))
					}
				}
			}
		})
	}
}
