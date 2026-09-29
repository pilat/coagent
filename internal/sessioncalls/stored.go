package sessioncalls

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type storedAdapter struct {
	store     sessionstore.RuntimeStore
	sessionID int64
	mu        sync.RWMutex
	cached    []llmwire.Message
}

// OpenStored loads a call owner using only the durable runtime transcript.
func OpenStored(
	ctx context.Context,
	store sessionstore.RuntimeStore,
	sessionID int64,
	stagedCalls map[string]string,
) (TranscriptSession, error) {
	if store == nil || sessionID == 0 {
		return nil, errors.New("transcript store and session ID are required")
	}

	adapter := &storedAdapter{store: store, sessionID: sessionID}
	if _, err := adapter.Messages(ctx); err != nil {
		return nil, fmt.Errorf("load transcript: %w", err)
	}

	return NewOwner(TranscriptAdapter{
		Messages: adapter.Messages, View: adapter.View,
		InsertResult: adapter.InsertResult, Reload: adapter.Reload,
	}, func() map[string]string { return stagedCalls }, nil), nil
}

func (a *storedAdapter) Messages(ctx context.Context) ([]llmwire.Message, error) {
	stored, err := a.store.LoadActiveMessages(ctx, a.sessionID)
	if err != nil {
		return nil, fmt.Errorf("load active messages: %w", err)
	}

	messages, err := decodeStored(stored)
	if err != nil {
		return nil, err
	}

	if _, err := Scan(messages); err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.cached = slices.Clone(messages)
	a.mu.Unlock()

	return messages, nil
}

func (a *storedAdapter) View() ([]llmwire.Message, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return slices.Clone(a.cached), nil
}

func (a *storedAdapter) InsertResult(ctx context.Context, call PendingToolCall, content string, toolError bool) error {
	stored, err := a.store.LoadActiveMessages(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("load tool call owner: %w", err)
	}
	messages, err := decodeStored(stored)
	if err != nil {
		return err
	}
	rowIDs := make([]int64, len(stored))
	for i, message := range stored {
		rowIDs[i] = message.ID
	}
	ref, err := LatestCallRef(messages, rowIDs, call.ID, call.Name)
	if err != nil {
		return fmt.Errorf("resolve stored call owner: %w", err)
	}
	_, _, err = a.store.InsertToolResultSetOnce(ctx, a.sessionID, []sessionstore.ToolResultEntry{{
		CallRef: ref,
		Message: &transcript.Message{
			Role:       llmwire.RoleTool,
			Content:    content,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			ToolError:  toolError,
		},
	}})
	if err != nil {
		return fmt.Errorf("persist tool result: %w", err)
	}

	return a.Reload(ctx)
}

func (a *storedAdapter) Reload(ctx context.Context) error {
	_, err := a.Messages(ctx)
	return err
}
