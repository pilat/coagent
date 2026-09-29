package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/transcript"
)

func toolResultEntry(
	ctx context.Context,
	store interface {
		InsertMessage(context.Context, int64, *transcript.Message) (int64, error)
	},
	sessionID int64,
	message *transcript.Message,
	directMessages []string,
) (ToolResultEntry, error) {

	calls, err := json.Marshal([]struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{{ID: message.ToolCallID, Name: message.ToolName}})
	if err != nil {
		return ToolResultEntry{}, fmt.Errorf("marshal test tool call: %w", err)
	}
	assistantID, err := store.InsertMessage(ctx, sessionID, &transcript.Message{
		Role: "assistant", ToolCalls: calls,
	})
	if err != nil {
		return ToolResultEntry{}, fmt.Errorf("insert test assistant call: %w", err)
	}

	return ToolResultEntry{
		CallRef:        CallRef{AssistantMessageID: assistantID},
		Message:        message,
		DirectMessages: directMessages,
	}, nil
}

func insertSingleToolResult(
	ctx context.Context,
	store interface {
		DirectOutputStore
		InsertMessage(context.Context, int64, *transcript.Message) (int64, error)
	},
	sessionID int64,
	message *transcript.Message,
	directMessages []string,
) (int64, []*OutputCommit, error) {
	entry, err := toolResultEntry(ctx, store, sessionID, message, directMessages)
	if err != nil {
		return 0, nil, err
	}

	return insertToolResultEntry(ctx, store, sessionID, entry)
}

func insertToolResultEntry(
	ctx context.Context,
	store DirectOutputStore,
	sessionID int64,
	entry ToolResultEntry,
) (int64, []*OutputCommit, error) {
	ids, outputs, err := store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
	if err != nil {
		return 0, nil, err
	}

	return ids[0], outputs[0], nil
}
