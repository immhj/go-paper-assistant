package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
	guardprovider "paperassistant/internal/provider/guard"
)

// stepOutcome is what a plan-capable tool reports about the step it just ran. A tool that ends
// in an expected state needing the user (no results, no full text, unknown ordinal) returns nil
// without marking success, and the plan stops there (ADR 0021 decision 7).
type stepOutcome struct {
	succeeded bool
	search    *searchChannelReply
}

type stepOutcomeKey struct{}

func withStepOutcome(ctx context.Context, outcome *stepOutcome) context.Context {
	return context.WithValue(ctx, stepOutcomeKey{}, outcome)
}

// markStepSucceeded records that the current step delivered its intended result.
func markStepSucceeded(ctx context.Context) {
	if outcome, ok := ctx.Value(stepOutcomeKey{}).(*stepOutcome); ok {
		outcome.succeeded = true
	}
}

// markSearchDelivered records the candidate list this channel actually showed, which is what a
// later step's selector picks from.
func markSearchDelivered(ctx context.Context, deliver deliverer, reply searchReply) {
	outcome, ok := ctx.Value(stepOutcomeKey{}).(*stepOutcome)
	if !ok {
		return
	}
	rendering := reply.QQ
	if picker, ok := deliver.(searchRenderingPicker); ok {
		rendering = picker.searchRendering(reply)
	}
	outcome.succeeded = true
	outcome.search = &rendering
}

// searchRenderingPicker is implemented by deliverers that do not show the QQ rendering.
type searchRenderingPicker interface {
	searchRendering(searchReply) searchChannelReply
}

// stepAnnouncer is implemented by deliverers that label progress with "步骤 i/n".
type stepAnnouncer interface {
	beginStep(index, total int)
}

// planCapableTools mark their own success; other tools only ever run as a one-step plan.
func planCapable(tool string) bool {
	return tool == "search_papers" || paperStepTools[tool]
}

// executePlan runs a validated plan step by step. It never hands a result back to the model:
// selectors are resolved here from the list the search step displayed. It stops at the first
// failed or stopped step and says which steps were not run. The returned trace always has one
// entry per planned step.
func (a *Application) executePlan(ctx context.Context, user domain.User, conversation domain.Conversation, input qq.IncomingMessage, steps []planStep, deliver deliverer) ([]domain.PlanStepTrace, error) {
	traces := make([]domain.PlanStepTrace, len(steps))
	for index, step := range steps {
		traces[index] = domain.PlanStepTrace{Tool: step.Tool, Args: step.Args, Select: step.Select, Status: "skipped"}
	}
	multi := len(steps) > 1
	var listed *searchChannelReply
	// failStep ends the plan on an error and still tells the user which steps did not run.
	failStep := func(index int, err error) ([]domain.PlanStepTrace, error) {
		traces[index].Status = "failed"
		if rest := steps[index+1:]; multi && len(rest) > 0 {
			noteErr := deliver.Text(context.WithoutCancel(ctx), conversation, conversationHeader(conversation)+"\n"+strings.TrimPrefix(notRunNote(rest), "\n"), nil, 0)
			return traces, errors.Join(err, noteErr)
		}
		return traces, err
	}
	for index, step := range steps {
		if announcer, ok := deliver.(stepAnnouncer); ok && multi {
			announcer.beginStep(index+1, len(steps))
		}
		args := step.Args
		note := ""
		if step.Select != nil {
			ordinals, weakNote, stop := selectFromListed(listed, step)
			if stop != "" {
				traces[index].Status = "stopped"
				return traces, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+stop+notRunNote(steps[index+1:]), nil, 0)
			}
			var err error
			if args, err = withSelectedOrdinals(step.Tool, args, ordinals); err != nil {
				return failStep(index, err)
			}
			traces[index].Args, note = args, weakNote
		}
		stepCtx := withStepOutcome(ctx, &stepOutcome{})
		outcome := stepCtx.Value(stepOutcomeKey{}).(*stepOutcome)
		if multi && step.Tool == "search_papers" {
			// Only the search part of the message carries the user's literal count; the data
			// source is still chosen from the whole message.
			stepCtx = withOverrideText(stepCtx, searchSegment(input.Content))
		}
		err := a.executeToolCall(stepCtx, user, conversation, input, deepseek.ToolCall{Name: step.Tool, Arguments: string(args)}, deliver)
		traces[index].Usage = a.takeToolUsage(input.MessageID)
		if err != nil && index > 0 && isQuotaRefusal(err) {
			// Running out of quota after earlier steps delivered is an expected stop, not a
			// failed task: the earlier results stay and the user learns why the rest is missing.
			traces[index].Status, traces[index].ErrorCode = "stopped", webErrorCode(err)
			return traces, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+webErrorMessage(err)+notRunNote(steps[index:]), nil, 0)
		}
		if err != nil {
			return failStep(index, err)
		}
		if note != "" && outcome.succeeded {
			if err = deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n"+note, nil, 0); err != nil {
				return failStep(index, err)
			}
		}
		if planCapable(step.Tool) && !outcome.succeeded {
			traces[index].Status = "stopped"
			if rest := steps[index+1:]; len(rest) > 0 {
				return traces, deliver.Text(ctx, conversation, conversationHeader(conversation)+"\n上一步没有完成，"+strings.TrimPrefix(notRunNote(rest), "\n"), nil, 0)
			}
			return traces, nil
		}
		traces[index].Status = "succeeded"
		if outcome.search != nil {
			listed = outcome.search
			conversation.CurrentContext = outcome.search.Context
		}
	}
	return traces, nil
}

type overrideTextKey struct{}

// withOverrideText sets the text the deterministic search overrides read for this step.
func withOverrideText(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, overrideTextKey{}, text)
}

// overrideText returns the step's override text, defaulting to the whole user message.
func overrideText(ctx context.Context, fallback string) string {
	if text, ok := ctx.Value(overrideTextKey{}).(string); ok {
		return text
	}
	return fallback
}

// isQuotaRefusal reports a per-tool gate refusal: rate limit or a guest read already used.
func isQuotaRefusal(err error) bool {
	return errors.Is(err, guardprovider.ErrRateLimited) || errors.Is(err, dao.ErrGuestDeepReadExhausted) || errors.Is(err, dao.ErrGuestTaskInProgress)
}

// selectFromListed resolves a selector against the displayed list. It returns the conversation
// ordinals, an optional note about weak relevance, or a stop reason when too few papers exist.
func selectFromListed(listed *searchChannelReply, step planStep) ([]int, string, string) {
	positions, err := selectorPositions(*step.Select)
	if err != nil {
		return nil, "", err.Error()
	}
	shown := 0
	if listed != nil {
		shown = len(listed.Shown)
	}
	for _, position := range positions {
		if position > shown {
			return nil, "", fmt.Sprintf("上一步只展示了 %d 篇论文，%s需要的论文不足，因此没有执行%s。", shown, toolLabel(step.Tool), toolLabel(step.Tool))
		}
	}
	ordinals := make([]int, 0, len(positions))
	weak := make([]string, 0)
	for _, position := range positions {
		paper := listed.Shown[position-1]
		ordinals = append(ordinals, paper.Ordinal)
		if paper.Relevance == nil {
			weak = append(weak, fmt.Sprintf("#%d 相关性未判断", paper.Ordinal))
		} else if *paper.Relevance == "low" {
			weak = append(weak, fmt.Sprintf("#%d 相关性为低", paper.Ordinal))
		}
	}
	note := ""
	if len(weak) > 0 {
		note = fmt.Sprintf("说明：上面的%s所用论文中 %s，已按你的要求执行。", toolLabel(step.Tool), strings.Join(weak, "、"))
	}
	return ordinals, note, ""
}

func notRunNote(rest []planStep) string {
	if len(rest) == 0 {
		return ""
	}
	labels := make([]string, 0, len(rest))
	for _, step := range rest {
		labels = append(labels, toolLabel(step.Tool))
	}
	return "\n以下步骤没有执行：" + strings.Join(labels, "、") + "。需要时请单独发送。"
}

// planTrace folds executed step traces into the turn trace.
func planTrace(trace *domain.AgentTrace, steps []domain.PlanStepTrace) {
	trace.Plan = steps
	if len(steps) > 0 {
		trace.Tool = steps[0].Tool
	}
	trace.ToolUsage = domain.ModelUsage{}
	for _, step := range steps {
		trace.ToolUsage.Add(step.Usage)
	}
}

// stepErrorCode labels a failed step in the trace with the channel-neutral error class.
func stepErrorCode(steps []domain.PlanStepTrace, err error) {
	if err == nil {
		return
	}
	for index := range steps {
		if steps[index].Status == "failed" {
			steps[index].ErrorCode = webErrorCode(err)
		}
	}
}
