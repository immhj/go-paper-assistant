package service

import (
	"context"
	"encoding/json"
	"fmt"

	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
	"paperassistant/internal/provider/deepseek"
)

// deliverer is the channel-neutral output seam for a turn. The paper-assistant tools produce
// text, candidate lists and errors and hand them to a deliverer; the deliverer decides how they
// reach the user. QQ streams them under the passive-reply quota (qqDeliverer); the Web channel
// emits SSE frames (see Spec 0002 / ADR 0013). Conversation is passed per call because a single
// turn can deliver against a freshly created/switched conversation (e.g. create_conversation).
type deliverer interface {
	// Text delivers a discrete message: progress lines, informational replies, and candidate
	// "no results" notices (which carry an empty shown slice).
	Text(ctx context.Context, conversation domain.Conversation, text string, shown []domain.ShownPaper, cost float64) error
	// Stream delivers a long final answer (abstract, comparison, research answer).
	Stream(ctx context.Context, conversation domain.Conversation, text string, cost float64) error
	// Read delivers a deep/quick read result. paperID/readType are explicit so a Web
	// deliverer can atomically attach the result while QQ deliberately ignores project state.
	Read(ctx context.Context, conversation domain.Conversation, paperID uint64, readType, text string, cost float64) error
	// SearchResult delivers a candidate list and atomically persists the updated conversation
	// context (ordinals) together with the shown-papers record.
	// Each channel picks its own rendering from reply.
	SearchResult(ctx context.Context, conversation domain.Conversation, reply searchReply, cost float64) error
}

// searchReply carries both channel renderings of one search. QQ is a single text message under
// a length budget and may show fewer papers; the Web shows every candidate as a card beside a
// one-line summary. Each rendering carries the conversation context recording exactly the
// ordinals it delivers, so /read never resolves a candidate that channel did not show.
type searchReply struct {
	QQ  searchChannelReply
	Web searchChannelReply
}

type searchChannelReply struct {
	Text    string
	Shown   []domain.ShownPaper
	Context json.RawMessage
}

// liveProgressReporter is an optional channel capability: a deliverer that already shows task
// progress live (Web SSE) does not need tools to send separate "working on it" messages.
type liveProgressReporter interface {
	reportsLiveProgress() bool
	reportProgress(code, text string) error
}

// announce tells the user a long step has started. A channel with a live progress indicator
// shows liveText there and persists nothing (an empty liveText leaves the indicator as is);
// other channels receive text as an ordinary message.
func announce(ctx context.Context, deliver deliverer, conversation domain.Conversation, code, liveText, text string) error {
	if reporter, ok := deliver.(liveProgressReporter); ok && reporter.reportsLiveProgress() {
		if liveText == "" {
			return nil
		}
		return reporter.reportProgress(code, liveText)
	}
	return deliver.Text(ctx, conversation, text, nil, 0)
}

// modelStreamDeliverer is an optional channel capability. The Web deliverer implements it to
// forward model deltas immediately; QQ deliberately keeps its existing complete-then-replace
// behavior because its passive-reply stream has different sequencing and quota semantics.
type modelStreamDeliverer interface {
	ModelStream(
		ctx context.Context,
		conversation domain.Conversation,
		paperID uint64,
		readType, prefix, suffix string,
		stream func(func(string) error) (domain.ModelUsage, error),
	) (domain.ModelUsage, error)
}

func (a *Application) streamModelToWeb(
	ctx context.Context,
	conversation domain.Conversation,
	request deepseek.Request,
	paperID uint64,
	readType, prefix, suffix string,
	deliver deliverer,
) (domain.ModelUsage, bool, error) {
	streamDeliverer, ok := deliver.(modelStreamDeliverer)
	if !ok {
		return domain.ModelUsage{}, false, nil
	}
	usage, err := streamDeliverer.ModelStream(ctx, conversation, paperID, readType, prefix, suffix, func(onDelta func(string) error) (domain.ModelUsage, error) {
		return a.model.Stream(ctx, request, onDelta)
	})
	return usage, true, err
}

// qqDeliverer routes the deliverer seam back onto the existing QQ delivery methods, unchanged.
// It carries the QQ-specific identity (the inbound message and the owning QQ user) that those
// methods need for the passive-reply quota, sequence numbers and message persistence.
type qqDeliverer struct {
	app    *Application
	input  qq.IncomingMessage
	userID uint64
}

func (d qqDeliverer) Text(ctx context.Context, conversation domain.Conversation, text string, shown []domain.ShownPaper, cost float64) error {
	return d.app.replyText(ctx, conversation, d.input, text, shown, cost)
}

func (d qqDeliverer) Stream(ctx context.Context, conversation domain.Conversation, text string, cost float64) error {
	return d.app.replyGeneratedText(ctx, conversation, d.input, text, cost)
}

func (d qqDeliverer) Read(ctx context.Context, conversation domain.Conversation, _ uint64, _, text string, cost float64) error {
	return d.app.deliverReadResult(ctx, conversation, d.input, text, cost)
}

func (d qqDeliverer) SearchResult(ctx context.Context, conversation domain.Conversation, reply searchReply, cost float64) error {
	return d.app.deliverSearchResultQQ(ctx, conversation, d.input, d.userID, reply.QQ.Context, reply.QQ.Text, reply.QQ.Shown, cost)
}

// deliverSearchResultQQ is the QQ candidate delivery lifted verbatim from the search tool: it
// reserves a bot message under the passive-reply quota, sends the replace-stream, then persists
// the updated conversation context and shown papers atomically so a delivered candidate list is
// never lost and /read never resolves an undelivered candidate.
func (a *Application) deliverSearchResultQQ(ctx context.Context, conversation domain.Conversation, input qq.IncomingMessage, userID uint64, currentContext json.RawMessage, response string, visible []domain.ShownPaper, cost float64) error {
	shownJSON, _ := json.Marshal(visible)
	count, err := a.store.CountBotReplies(ctx, input.MessageID)
	if err != nil {
		return err
	}
	if count >= 4 {
		return errQuotaExhausted
	}
	seq := uint32(count + 1)
	content, qqID := response, input.MessageID
	reserved, err := a.store.InsertMessage(ctx, domain.Message{
		ConversationID: conversation.ID, Role: "bot", QQMessageID: &qqID, MessageSeq: &seq,
		Content: &content, ShownPapers: shownJSON, CostUSD: 0, DeliveryStatus: "reserved",
	})
	if err != nil {
		return err
	}
	outboundID, sendErr := a.sendFinalStreamWithSeq(ctx, input, seq, response)
	if sendErr != nil {
		_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "failed", outboundID)
		return sendErr
	}
	_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "reserved", outboundID)
	if err = a.saveSearchResultWithRetry(ctx, userID, conversation.ID, currentContext, domain.Message{
		ID: reserved.ID, ConversationID: conversation.ID, Role: "bot", QQMessageID: &qqID, MessageSeq: &seq,
		Content: &content, ShownPapers: shownJSON, CostUSD: cost,
	}); err != nil {
		_ = a.store.UpdateMessageDelivery(ctx, reserved.ID, "failed", outboundID)
		return fmt.Errorf("candidate stream was delivered but result persistence failed: %w", err)
	}
	return nil
}

// errQuotaExhausted is the shared sentinel for exceeding the QQ passive-reply limit; only the
// QQ channel raises it (the Web channel has no such quota, ADR 0013).
var errQuotaExhausted = errQuota("QQ passive reply quota exhausted")

type errQuota string

func (e errQuota) Error() string { return string(e) }
