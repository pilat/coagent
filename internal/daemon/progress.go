//nolint:wrapcheck // Snapshot assembly preserves durable projection errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

func (s *svc) CurrentProgress(
	ctx context.Context,
	rootID int64,
) (*controllerapi.ProgressData, error) {
	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return nil, errors.New("progress store unavailable")
	}

	facts, err := store.CaptureProgress(ctx, rootID)
	if err != nil {
		return nil, err
	}

	now := s.progressNow().UTC()

	snapshot, err := s.progressSnapshot(facts, now)
	if err != nil {
		return nil, err
	}

	if contextProjection, ok := s.liveContextProjection(ctx, facts.RootID); ok {
		snapshot.Context = progress.Context{
			Used: contextProjection.Used, Max: contextProjection.Max,
			Approximate: contextProjection.Approximate, Available: contextProjection.Available,
		}
	}

	return &controllerapi.ProgressData{
		SessionID: rootID, Revision: snapshot.Revision, OutboxWatermark: snapshot.OutboxWatermark,
		ObservedAt: now, Rendered: progress.RenderFull(snapshot, logger.Redact),
	}, nil
}

func (s *svc) RefreshProgress(ctx context.Context, rootID int64) error {
	_, err := s.enqueueProgressChange(ctx, rootID)
	if err == nil {
		s.wakeProgress()
	}

	return err
}

func (s *svc) renderFinalOutput(ctx context.Context, rootID int64, text string) (string, error) {
	store, ok := s.sessionStore.(sessionstore.ProgressStore)
	if !ok {
		return text, nil
	}

	facts, err := store.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", fmt.Errorf("capture final progress: %w", err)
	}

	snapshot, err := s.progressSnapshot(facts, s.progressNow().UTC())
	if err != nil {
		return "", err
	}

	footer := progress.RenderFooter(snapshot, logger.Redact)
	if footer == "" {
		return text, nil
	}

	if text == "" {
		return footer, nil
	}

	return text + "\n\n" + footer, nil
}

func (s *svc) progressSnapshot(
	facts *sessionstore.ProgressFacts,
	observedAt time.Time,
) (progress.Snapshot, error) {
	var todoItems []*todo.Item
	if err := json.Unmarshal(facts.TodoItems, &todoItems); err != nil {
		return progress.Snapshot{}, fmt.Errorf("decode progress todo: %w", err)
	}

	snapshot := progress.Snapshot{
		RootID: facts.RootID, DurableWatermark: facts.MessageWatermark,
		OutboxWatermark: facts.OutboxWatermark, PersistedReason: string(facts.Status),
		ObservedAt: observedAt, Model: facts.Model, RootIteration: facts.Iteration,
		ChildCount: facts.ChildCount, ChildIterations: facts.ChildIterations,
		Lifetime: progress.Usage{
			PromptTokens:     facts.PromptTokens,
			CompletionTokens: facts.CompletionTokens, CostUSD: facts.CostUSD, Available: true,
		},
		LatestModelProgress:  facts.LatestModelProgress,
		LastSemanticOutputAt: facts.LastSemanticOutputAt,
		ActiveSubagents:      facts.ActiveSubagents, BackgroundSubagents: facts.BackgroundSubagents,
	}
	if s.HasActiveLoop(facts.RootID) {
		snapshot.RuntimeState = "running"
	} else {
		snapshot.RuntimeState = "idle"
	}

	if facts.EpisodeStartedAt != nil && !observedAt.Before(*facts.EpisodeStartedAt) {
		elapsed := observedAt.Sub(*facts.EpisodeStartedAt)
		snapshot.EpisodeElapsed = &elapsed
	}

	for _, item := range todoItems {
		snapshot.Todos = append(snapshot.Todos, progress.TodoItem{
			ID: item.ID, Content: item.Content, Status: string(item.Status), Priority: string(item.Priority),
		})
	}

	for _, wait := range facts.Waiting {
		snapshot.Waiting = append(snapshot.Waiting, progress.WaitingItem{
			Kind: wait.Kind, Description: wait.Description, WakeAt: wait.WakeAt,
		})
	}

	snapshot.Budget = progressBudget(facts.Budget, facts.CostUSD, observedAt)

	payload, err := json.Marshal(snapshot) //nolint:musttag // Internal closed struct is not a wire contract.
	if err != nil {
		return progress.Snapshot{}, fmt.Errorf("marshal progress revision: %w", err)
	}

	digest := sha256.Sum256(payload)
	snapshot.Revision = hex.EncodeToString(digest[:])

	return snapshot, nil
}

//nolint:wsl_v5 // Runtime lookup is guarded at each ownership boundary.
func (s *svc) liveContextProjection(ctx context.Context, rootID int64) (session.ContextProjection, bool) {
	s.mu.Lock()
	runner := s.loops[rootID]
	s.mu.Unlock()
	if runner == nil {
		return session.ContextProjection{}, false
	}

	runner.svcMu.Lock()
	service := runner.service
	runner.svcMu.Unlock()
	if service == nil {
		return session.ContextProjection{}, false
	}

	provider, ok := service.(interface {
		ContextProjection(context.Context) session.ContextProjection
	})
	if !ok {
		return session.ContextProjection{}, false
	}

	return provider.ContextProjection(ctx), true
}

//nolint:wsl_v5 // Derived budget fields are assembled as one projection.
func progressBudget(record *sessionstore.BudgetRecord, cost float64, now time.Time) *progress.Budget {
	if record == nil {
		return nil
	}

	value := &progress.Budget{
		State: string(record.State), Generation: record.Generation,
		CostLimitUSD: record.CostLimitUSD, FiredReason: record.FiredReason,
	}
	used := cost - record.BaselineCostUSD
	value.CostUsedUSD = &used
	if record.CostLimitUSD != nil {
		remaining := max(*record.CostLimitUSD-used, 0)
		value.CostRemainingUSD = &remaining
	}

	if record.DurationSeconds != nil {
		limit := time.Duration(*record.DurationSeconds) * time.Second
		value.DurationLimit = &limit

		if !now.Before(record.ArmedAt) {
			elapsed := now.Sub(record.ArmedAt)
			value.Elapsed = &elapsed
			remaining := max(limit-elapsed, 0)
			value.DurationRemaining = &remaining
		}
	}

	return value
}
