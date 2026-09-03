//nolint:wrapcheck // Snapshot assembly preserves durable projection errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package progressruntime

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
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

func (r *runtime) current(
	ctx context.Context,
	rootID int64,
) (*controllerapi.ProgressData, error) {
	facts, err := r.sessionStore.CaptureProgress(ctx, rootID)
	if err != nil {
		return nil, err
	}

	now := r.progressNow().UTC()

	snapshot, err := r.progressSnapshot(facts, now)
	if err != nil {
		return nil, err
	}

	if contextProjection, ok := r.liveContextProjection(ctx, facts.RootID); ok {
		snapshot.Context = contextProjection
	}

	return &controllerapi.ProgressData{
		SessionID: rootID, Revision: snapshot.Revision, OutboxWatermark: snapshot.OutboxWatermark,
		ObservedAt: now, Rendered: progress.RenderFull(snapshot, logger.Redact),
	}, nil
}

func (r *runtime) refresh(ctx context.Context, rootID int64) error {
	_, _, err := r.enqueueProgressChange(ctx, rootID)
	// Supersession is successful suppression, never a failure: a manager that
	// refreshes a just-fenced root must not treat the stale snapshot as an error.
	if errors.Is(err, sessionstore.ErrProgressSuperseded) {
		return nil
	}

	if err == nil {
		r.wakeProgress()
	}

	return err
}

func (r *runtime) renderFinalOutput(ctx context.Context, rootID int64, text string) (string, error) {
	facts, err := r.sessionStore.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", fmt.Errorf("capture final progress: %w", err)
	}

	snapshot, err := r.progressSnapshot(facts, r.progressNow().UTC())
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

func (r *runtime) progressSnapshot(
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
		MainModelWorking: r.mainModelWorking(facts.RootID),
		ChildCount:       facts.ChildCount, ChildIterations: facts.ChildIterations,
		Lifetime: progress.Usage{
			PromptTokens:     facts.PromptTokens,
			CompletionTokens: facts.CompletionTokens, CostUSD: facts.CostUSD, Available: true,
		},
		LatestModelProgress:  facts.LatestModelProgress,
		LastSemanticOutputAt: facts.LastSemanticOutputAt,
		ActiveSubagents:      facts.ActiveSubagents, BackgroundSubagents: facts.BackgroundSubagents,
	}
	if r.hasActiveLoop(facts.RootID) {
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
