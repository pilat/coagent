package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/tool"
)

const (
	memoryMaxCount   = 50
	memoryMaxTextLen = 200

	memorySaveTitle = "Memory save"
)

var _ tool.Tool = (*memorySaveTool)(nil)

// memorySaveParams are the parameters for the memory_save tool.
type memorySaveParams struct {
	Text string `json:"text"`
}

type memorySaveTool struct {
	store     memory.CuratedStore
	projectID int64
	onChanged func(context.Context) // callback to refresh session's memoriesSection
}

func NewMemorySaveTool(store memory.CuratedStore, projectID int64, onChanged func(context.Context)) tool.Tool {
	return &memorySaveTool{store: store, projectID: projectID, onChanged: onChanged}
}

func (t *memorySaveTool) ID() string { return "memory_save" }

func (t *memorySaveTool) Description() string {
	return fmt.Sprintf(`Save a short per-project memory (max %d chars, max %d per project).

Use when the user asks you to remember something. Memories are injected into every system prompt.`, memoryMaxTextLen, memoryMaxCount)
}

func (t *memorySaveTool) Parameters() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"text": {
				"type": "string",
				"description": "The memory text to save (max %d characters).",
				"maxLength": %d
			}
		},
		"required": ["text"]
	}`, memoryMaxTextLen, memoryMaxTextLen))
}

func (t *memorySaveTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p memorySaveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Text == "" {
		return nil, errors.New("text is required")
	}

	if len(p.Text) > memoryMaxTextLen {
		return &tool.Result{
			Title:  memorySaveTitle,
			Output: fmt.Sprintf("Text too long (%d chars, max %d). Please shorten it.", len(p.Text), memoryMaxTextLen),
		}, nil
	}

	count, err := t.store.CountMemories(ctx, t.projectID)
	if err != nil {
		return nil, fmt.Errorf("count memories: %w", err)
	}

	if count >= memoryMaxCount {
		return &tool.Result{
			Title: memorySaveTitle,
			Output: fmt.Sprintf("Memory limit reached (%d/%d). Consider consolidating related memories. "+
				"Ask the user before modifying existing memories.", count, memoryMaxCount),
		}, nil
	}

	id, err := t.store.SaveMemory(ctx, t.projectID, p.Text)
	if err != nil {
		return nil, fmt.Errorf("save memory: %w", err)
	}

	if t.onChanged != nil {
		t.onChanged(ctx)
	}

	return &tool.Result{
		Title:  memorySaveTitle,
		Output: fmt.Sprintf("Saved memory %d (%d/%d): %s", id, count+1, memoryMaxCount, p.Text),
	}, nil
}
