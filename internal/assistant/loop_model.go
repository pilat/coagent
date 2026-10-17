package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
)

func (l *loop) ask(ctx context.Context, s *step) (bool, error) {
	for attempt := range 3 {
		if err := l.loadStep(ctx, s); err != nil {
			return false, err
		}

		raw, err := l.chat(ctx, s.render)
		s.raw = raw

		if ctx.Err() != nil {
			return false, fmt.Errorf("assistant model interrupted: %w", ctx.Err())
		}

		if err != nil {
			return false, l.rejectStep(ctx, *s, err.Error(), true)
		}

		s.answer, err = Parse(raw)
		if err == nil {
			s.next = s.desk.Edited(s.answer, l.deps.Now())

			existing := map[string]bool{}
			for _, note := range s.desk.Notes {
				existing[note.Key] = true
			}

			var reasons []string
			for _, v := range Validate(s.answer, s.next, DefaultRules(), ValidationContext{
				OriginKind: s.originKind, Shown: s.shown, ProjectKeys: s.shown.ProjectKeys, ExistingKeys: existing,
			}) {
				reasons = append(reasons, string(v))
			}

			// The last step must answer; a read here would end the turn in silence.
			if s.number == 3 && slices.ContainsFunc(s.answer.Do, func(a Action) bool { return a.Read != nil }) {
				reasons = append(reasons, "no steps left in this turn: answer now, without read")
			}

			if len(reasons) > 0 {
				err = errors.New(strings.Join(reasons, "; "))
			}
		}

		if err == nil {
			return true, nil
		}

		if err := l.rejectStep(ctx, *s, err.Error(), attempt == 2); err != nil {
			return false, err
		}
	}

	return false, nil
}

func (l *loop) loadStep(ctx context.Context, s *step) error {
	var err error

	s.desk, err = l.deps.Memory.LoadDesk(ctx, l.sessionID)
	if err != nil {
		return fmt.Errorf("load step desk: %w", err)
	}

	s.catalog, err = l.deps.Sessions.Catalog(ctx, s.desk.LastTurnAt, s.event.Sessions...)
	if err != nil {
		return fmt.Errorf("load assistant catalog: %w", err)
	}

	now := l.deps.Now()
	s.shown = TrimCatalog(s.catalog, s.desk, s.event, now)
	s.cards = s.shown.Cards

	s.tape, err = l.deps.Memory.VisibleTape(ctx, l.sessionID, 40, s.event.LineID)
	if err != nil {
		return fmt.Errorf("load step tape: %w", err)
	}

	s.render = Render(s.desk, s.catalog, s.tape, s.event, now)

	return nil
}

func (l *loop) chat(ctx context.Context, desk string) (string, error) {
	var last error
	delays := []time.Duration{0, 2 * time.Second, 5 * time.Second, 15 * time.Second}

	for _, delay := range delays {
		if err := waitRetry(ctx, delay); err != nil {
			return "", err
		}

		raw, err := l.chatOnce(ctx, desk)
		if err == nil {
			return raw, nil
		}

		last = err
	}

	return "", fmt.Errorf("assistant model retries exhausted: %w", last)
}

func (l *loop) chatOnce(ctx context.Context, desk string) (string, error) {
	log := logger.Ctx(ctx).Named("assistant.chat")

	record, err := l.deps.Store.GetSession(ctx, l.sessionID)
	if err != nil {
		return "", fmt.Errorf("load assistant model: %w", err)
	}

	client, err := l.deps.NewClient(record.Model)
	if err != nil {
		return "", fmt.Errorf("build assistant model: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn("close model", zap.Error(err))
		}
	}()

	response, err := client.Chat(ctx, registry.AssistantPrompt,
		[]llmwire.Message{{Role: llmwire.RoleUser, Content: desk}}, nil)
	if err != nil {
		return "", fmt.Errorf("call assistant model: %w", err)
	}

	log.Info("step usage", zap.Any("usage", response.Usage), zap.Float64("cost_usd", response.CostUSD))

	return response.Text, nil
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("assistant retry interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
