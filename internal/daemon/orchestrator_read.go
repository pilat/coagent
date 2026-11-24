package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (h orchestratorHost) ReadSession(
	ctx context.Context,
	callerID, targetID int64,
	latest bool,
) (orchestrator.Page, error) {
	_, err := h.caller(ctx, callerID)
	if err != nil {
		return orchestrator.Page{}, err
	}

	if err := h.rootTarget(ctx, targetID); err != nil {
		return orchestrator.Page{}, err
	}

	chain, err := h.s.store.ReplacementChain(ctx, targetID)
	if err != nil {
		return orchestrator.Page{}, fmt.Errorf("resolve read target: %w", err)
	}

	resolved := chain[len(chain)-1]
	if resolved == callerID {
		return orchestrator.Page{}, errors.New("cannot read this session itself")
	}

	record, err := h.s.store.GetSession(ctx, resolved)
	if err != nil {
		return orchestrator.Page{}, fmt.Errorf("load read target: %w", err)
	}

	state, err := h.sessionState(ctx, record)
	if err != nil {
		return orchestrator.Page{}, err
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusTerminating {
		state = "killed"
	}

	page, err := h.s.store.ReadWatch(ctx, sessionstore.WatchRead{
		WatcherID: callerID, Chain: chain, Latest: latest,
		MaxRows: orchestrator.PageRows, MaxRunes: orchestrator.PageRunes,
	})
	if err != nil {
		return orchestrator.Page{}, fmt.Errorf("read session outputs: %w", err)
	}

	return readPage(targetID, resolved, state, page), nil
}

func readPage(requested, resolved int64, state string, page *sessionstore.WatchPage) orchestrator.Page {
	result := orchestrator.Page{
		Requested: requested, ID: resolved, Name: page.Name, State: state,
		Skipped: page.Skipped, Remaining: page.Remaining, RemainingAnswers: page.RemainingAnswers,
	}

	for _, row := range page.Rows {
		result.Rows = append(result.Rows, orchestrator.Row{
			ID: row.ID, Kind: row.Kind, Content: row.Content, Clipped: row.Clipped, At: row.CreatedAt,
		})
	}

	return result
}
