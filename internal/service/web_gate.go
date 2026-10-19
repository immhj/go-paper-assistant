package service

import (
	"context"
	"errors"
	"strings"

	"paperassistant/internal/dao"
	guardprovider "paperassistant/internal/provider/guard"
)

type toolGateContextKey struct{}

type toolGate interface {
	BeforeTool(context.Context, string) (func(error) error, error)
}

func withToolGate(ctx context.Context, gate toolGate) context.Context {
	return context.WithValue(ctx, toolGateContextKey{}, gate)
}

func toolGateFromContext(ctx context.Context) toolGate {
	gate, _ := ctx.Value(toolGateContextKey{}).(toolGate)
	return gate
}

func webToolProgress(toolName string) (string, string) {
	switch toolName {
	case "search_papers":
		return "tool.search", "正在执行：论文检索"
	case "read_paper_quick":
		return "tool.quick_read", "正在执行：快速精读"
	case "read_paper_deep":
		return "tool.deep_read", "正在执行：深度精读"
	case "compare_papers":
		return "tool.compare", "正在执行：论文对比"
	case "answer_research_question", "answer_from_current_project_papers":
		return "tool.research", "正在执行：研究问答"
	case "get_paper_abstract":
		return "tool.abstract", "正在执行：读取摘要"
	default:
		return "tool", "正在执行任务"
	}
}

type webToolGate struct {
	app     *Application
	actor   WebActor
	deliver *webDeliverer
}

func (g *webToolGate) BeforeTool(ctx context.Context, toolName string) (func(error) error, error) {
	if g.app.webGuard != nil {
		var class guardprovider.ToolClass
		switch toolName {
		case "search_papers":
			class = guardprovider.ToolSearchOrQuick
		case "read_paper_quick":
			if g.actor.Type == WebActorAccount {
				class = guardprovider.ToolAccountDeep
			} else {
				class = guardprovider.ToolSearchOrQuick
			}
		case "read_paper_deep":
			if g.actor.Type == WebActorAccount {
				class = guardprovider.ToolAccountDeep
			}
		}
		if class != "" {
			if err := g.app.webGuard.AllowTool(ctx, g.actor.key(), class); err != nil {
				return nil, err
			}
		}
	}

	if g.deliver != nil && g.deliver.emit != nil && !g.deliver.startedAt.IsZero() {
		code, content := webToolProgress(toolName)
		if g.deliver.stepLabel != "" {
			content = g.deliver.stepLabel + strings.TrimPrefix(content, "正在执行：")
		}
		if err := g.deliver.progress(code, content); err != nil {
			return nil, err
		}
	}

	if g.actor.Type != WebActorGuest || (toolName != "read_paper_quick" && toolName != "read_paper_deep") {
		return nil, nil
	}
	if !g.app.guestDeepReadEnabled {
		return nil, dao.ErrGuestDeepReadExhausted
	}
	if g.app.platformStore == nil {
		return nil, guardprovider.ErrUnavailable
	}
	reservationID, err := randomHex(16)
	if err != nil {
		return nil, errors.Join(guardprovider.ErrUnavailable, err)
	}
	if _, err = g.app.platformStore.ReserveGuestDeepRead(ctx, g.actor.ID, reservationID); err != nil {
		return nil, err
	}
	g.deliver.guestReservationID = &reservationID
	return func(_ error) error {
		if g.deliver.readCommitted {
			return nil
		}
		return g.app.platformStore.FinishGuestDeepRead(context.WithoutCancel(ctx), g.actor.ID, reservationID, false)
	}, nil
}
