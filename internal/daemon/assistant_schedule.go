package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/tool"
)

func (s *svc) assistantScheduleTool(scope assistant.Scope) tool.Tool {
	resolve := func(ctx context.Context, id int64) (string, error) {
		card, err := s.assistant.Target(ctx, scope, id)
		if err != nil {
			return "", fmt.Errorf("resolve schedule target: %w", err)
		}

		return assistantScheduleName(card), nil
	}
	list := func(ctx context.Context) ([]schedule.Target, error) {
		cards, _, err := s.assistant.List(ctx, scope, 0, math.MaxInt, "")
		if err != nil {
			return nil, fmt.Errorf("list schedule targets: %w", err)
		}

		targets := make([]schedule.Target, 0, len(cards))
		for _, card := range cards {
			targets = append(targets, schedule.Target{ID: card.ID, Name: assistantScheduleName(card)})
		}

		return targets, nil
	}

	return schedule.NewTargetedScheduleTool(scope.Root, s.schedules, time.Local, resolve, list)
}

func assistantScheduleName(card assistant.Card) string {
	name := fmt.Sprintf("#%d %s", card.ID, card.Project)
	if card.Title != "" {
		name += " · " + card.Title
	}

	return name
}
