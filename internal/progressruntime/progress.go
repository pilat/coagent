package progressruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

func (r *Runtime) current(
	ctx context.Context,
	rootID int64,
) (string, error) {
	facts, err := r.sessionStore.CaptureProgress(ctx, rootID)
	if err != nil {
		return "", fmt.Errorf("capture status: %w", err)
	}

	now := time.Now().UTC()

	snapshot, err := r.progressSnapshot(facts, now)
	if err != nil {
		return "", err
	}

	if live := r.liveState(facts.RootID); live.Active {
		snapshot.Context = live.Context
	}

	return progress.RenderFull(snapshot), nil
}

func (r *Runtime) progressSnapshot(
	facts *sessionstore.ProgressFacts,
	observedAt time.Time,
) (progress.Snapshot, error) {
	var todoItems []*todo.Item
	if err := json.Unmarshal(facts.TodoItems, &todoItems); err != nil {
		return progress.Snapshot{}, fmt.Errorf("decode progress todo: %w", err)
	}

	// Durable items must reach /status in the same canonical order todoread
	// reports; sort before conversion because progress.TodoItem has no timestamps.
	todo.SortCanonical(todoItems)

	snapshot := progress.Snapshot{
		RootID: facts.RootID, PersistedReason: string(facts.Status),
		ObservedAt: observedAt, Model: facts.Model, RootIteration: facts.Iteration,
		ChildCount: facts.ChildCount, ChildIterations: facts.ChildIterations,
		Lifetime: progress.Usage{
			PromptTokens:     facts.PromptTokens,
			CompletionTokens: facts.CompletionTokens, CostUSD: facts.CostUSD, Available: true,
		},
		ActiveSubagents: facts.ActiveSubagents, BackgroundSubagents: facts.BackgroundSubagents,
	}
	if r.liveState(facts.RootID).Active {
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

	// Owner is rendered as "main" for the root session, else the subagent id.
	for _, process := range facts.BackgroundProcesses {
		owner := "main"
		if !process.IsRoot {
			owner = fmt.Sprintf("subagent %d", process.OwnerID)
		}

		snapshot.BackgroundProcesses = append(snapshot.BackgroundProcesses, progress.ProcessStatus{
			ProcessID: process.ProcessID, Owner: owner, State: process.State,
			Elapsed: observedAt.Sub(process.StartedAt), DeadlineAt: process.DeadlineAt,
			OutputSize: process.OutputSize,
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

func progressBudget(record *budget.Record, cost float64, now time.Time) *progress.Budget {
	if record == nil {
		return nil
	}

	value := &progress.Budget{
		State: string(record.State), Generation: record.Generation,
		CostLimitUSD: record.CostLimitUSD, FiredReason: record.FiredReason,
	}
	used := cost - record.BaselineCostUSD
	value.CostUsedUSD = &used
	value.CostRemainingUSD, value.DurationRemaining = record.Remaining(cost, now)

	if record.DurationSeconds != nil {
		limit := time.Duration(*record.DurationSeconds) * time.Second
		value.DurationLimit = &limit

		if !now.Before(record.ArmedAt) {
			elapsed := now.Sub(record.ArmedAt)
			value.Elapsed = &elapsed
		}
	}

	return value
}
