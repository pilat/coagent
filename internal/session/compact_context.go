package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

const compactionKeepRecentFloor = 4

// compactor is the session-local contract the compact_context tool drives.
// The session (*svc) implements it directly, so no cross-package interface is
// laundered back into the tool package.
type compactor interface {
	RequestCompaction(keepRecentRounds int)
}

type compactContextParams struct {
	KeepRecentRounds int `json:"keep_recent_rounds"`
}

type compactContextTool struct {
	compactor compactor
}

var (
	_ compactor = (*svc)(nil)
	_ tool.Tool = (*compactContextTool)(nil)
)

func newCompactContextTool(c compactor) *compactContextTool {
	return &compactContextTool{compactor: c}
}

func (t *compactContextTool) ID() string { return tool.IDCompactContext }

func (t *compactContextTool) Description() string {
	return `Replace the conversation with a written summary of it. Everything after the opening task is condensed — nothing is kept verbatim — so call this when starting a new phase of work and the earlier detail is no longer needed. Cannot be combined with a call that suspends the session (task, sleep, config changes); make those first and compact afterwards.`
}

func (t *compactContextTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"keep_recent_rounds": {
				"type": "integer",
				"description": "How many recent tool-call rounds the summarizer reads with their full tool output; older results reach it as placeholders (default: 6, min: 4, max: 20)"
			}
		}
	}`)
}

func (t *compactContextTool) Execute(_ context.Context, params json.RawMessage) (*tool.Result, error) {
	var p compactContextParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	keepRecent := p.KeepRecentRounds
	if keepRecent == 0 {
		keepRecent = 6 // default
	}

	if keepRecent < compactionKeepRecentFloor {
		return nil, fmt.Errorf("keep_recent_rounds must be at least %d, got %d", compactionKeepRecentFloor, keepRecent)
	}

	if keepRecent > 20 {
		return nil, fmt.Errorf("keep_recent_rounds must be at most 20, got %d", keepRecent)
	}

	compactor := t.compactor
	if compactor == nil {
		return nil, errors.New("compactor not configured")
	}

	compactor.RequestCompaction(keepRecent)

	return &tool.Result{
		Output: fmt.Sprintf(
			"Context compaction requested (keeping %d recent rounds). Will execute at next iteration.",
			keepRecent,
		),
	}, nil
}
