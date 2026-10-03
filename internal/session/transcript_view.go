package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// messageStore manages the agent's conversation message history with optional persistence.
type messageStore struct {
	mu       sync.Mutex
	messages []llmwire.Message
	rowIDs   []int64
	store    Store // nil = in-memory only (tests without persistence)
	sessID   int64 // session ID for persistence
}

// PendingToolCall identifies one exact suspended tool invocation.
type PendingToolCall struct {
	ID   string
	Name string
}

type toolCallStatus struct {
	name      string
	found     bool
	resolved  bool
	duplicate bool
}

// PendingExternalCalls is deliberately global over the active transcript.
// External work is causal state: a later user or synthetic event cannot
// supersede it merely by becoming the latest turn.
func (s *Session) PendingExternalCalls() []PendingToolCall {
	calls := unresolvedCallsMatching(s.ms.getMessages(), func(tc llmwire.ToolCall) bool {
		return s.stagedCalls[tc.ID] == tc.Name
	})

	result := make([]PendingToolCall, 0, len(calls))

	for _, call := range calls {
		result = append(result, PendingToolCall{ID: call.ID, Name: call.Name})
	}

	return result
}

func (s *Session) HasPendingExternalCall() bool {
	return len(s.PendingExternalCalls()) > 0
}

// HasPendingWork includes host continuation rows so restarts retain unfinished turns.
func (s *Session) HasPendingWork() bool {
	if s.HasPendingExternalCall() {
		return false
	}
	return s.unansweredWork()
}

func UnresolvedCalls(messages []llmwire.Message) []PendingToolCall {
	calls := unresolvedCallsMatching(messages, func(llmwire.ToolCall) bool { return true })
	result := make([]PendingToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, PendingToolCall{ID: call.ID, Name: call.Name})
	}
	return result
}

func SettleResults(calls []PendingToolCall, text string) []*transcript.Message {
	results := make([]*transcript.Message, 0, len(calls))
	for _, call := range calls {
		results = append(
			results,
			&transcript.Message{
				Role:       llmwire.RoleTool,
				Content:    text,
				ToolCallID: call.ID,
				ToolName:   call.Name,
				ToolError:  true,
			},
		)
	}
	return results
}

func newMessageStore(
	store Store,
	sessID int64,
) *messageStore {
	return &messageStore{
		messages: make([]llmwire.Message, 0),
		rowIDs:   make([]int64, 0),
		store:    store,
		sessID:   sessID,
	}
}

func compactionEntries(messages []llmwire.Message, rowIDs []int64) ([]sessionstore.CompactionEntry, error) {
	if len(messages) != len(rowIDs) {
		return nil, fmt.Errorf("serialize %d compaction messages with %d row ids", len(messages), len(rowIDs))
	}

	entries := make([]sessionstore.CompactionEntry, len(messages))
	for i := range messages {
		if rowIDs[i] != 0 {
			entries[i].ExistingID = rowIDs[i]

			continue
		}

		message, err := storedMessage(&messages[i])
		if err != nil {
			return nil, fmt.Errorf("serialize compaction message %d: %w", i, err)
		}

		entries[i].Message = message
	}

	return entries, nil
}

func (ms *messageStore) setMessages(msgs []llmwire.Message) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.messages = msgs
	ms.rowIDs = make([]int64, len(msgs))
}

func (ms *messageStore) setMessagesWithRowIDs(msgs []llmwire.Message, rowIDs []int64) error {
	if len(msgs) != len(rowIDs) {
		return fmt.Errorf("restore %d messages with %d row ids", len(msgs), len(rowIDs))
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.messages = msgs

	ms.rowIDs = append([]int64(nil), rowIDs...)

	return nil
}

func (ms *messageStore) getMessages() []llmwire.Message {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	result := make([]llmwire.Message, len(ms.messages))
	copy(result, ms.messages)

	return result
}

func (ms *messageStore) getRowIDs() []int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	return append([]int64(nil), ms.rowIDs...)
}

// reloadMessages replaces in-memory messages with active messages from the store.
// No-op when store is nil.
func (ms *messageStore) reloadMessages(ctx context.Context) error {
	if ms.store == nil {
		return nil
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	return ms.reloadMessagesLocked(ctx)
}

func (ms *messageStore) reloadMessagesLocked(ctx context.Context) error {
	stored, err := ms.store.LoadActiveMessages(ctx, ms.sessID)
	if err != nil {
		return fmt.Errorf("reload messages: %w", err)
	}

	messages := make([]llmwire.Message, len(stored))
	rowIDs := make([]int64, len(stored))

	for i, sm := range stored {
		msg := llmwire.Message{
			Role:                 sm.Role,
			Content:              sm.Content,
			ToolCallID:           sm.ToolCallID,
			ToolName:             sm.ToolName,
			ToolError:            sm.ToolError,
			ReasoningContent:     sm.ReasoningContent,
			ReasoningRaw:         sm.ReasoningRaw,
			CostUSD:              sm.CostUSD,
			FinishType:           sm.FinishType,
			ProviderFinishReason: sm.ProviderFinishReason,
		}

		if len(sm.ToolCalls) > 0 {
			if err := json.Unmarshal(sm.ToolCalls, &msg.ToolCalls); err != nil {
				return fmt.Errorf("unmarshal tool calls for message %d: %w", sm.ID, err)
			}
		}

		if len(sm.Attachments) > 0 {
			if err := json.Unmarshal(sm.Attachments, &msg.Images); err != nil {
				return fmt.Errorf("unmarshal attachments for message %d: %w", sm.ID, err)
			}
		}

		if len(sm.Usage) > 0 {
			var usage llmwire.MessageUsage
			if err := json.Unmarshal(sm.Usage, &usage); err != nil {
				return fmt.Errorf("unmarshal usage for message %d: %w", sm.ID, err)
			}

			msg.Usage = &usage
		}

		messages[i] = msg
		rowIDs[i] = sm.ID
	}

	ms.messages = messages
	ms.rowIDs = rowIDs

	return nil
}

func storedMessage(msg *llmwire.Message) (*transcript.Message, error) {
	var toolCallsJSON json.RawMessage

	if len(msg.ToolCalls) > 0 {
		data, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return nil, fmt.Errorf("marshal tool calls: %w", err)
		}

		toolCallsJSON = data
	}

	var usageJSON json.RawMessage

	if msg.Usage != nil {
		data, err := json.Marshal(msg.Usage)
		if err != nil {
			return nil, fmt.Errorf("marshal usage: %w", err)
		}

		usageJSON = data
	}

	var attachmentsJSON json.RawMessage

	// Images are valid on user/tool rows only (D2); any other role drops them
	// so they can neither render nor inflate the token estimate.
	if len(msg.Images) > 0 && (msg.Role == llmwire.RoleUser || msg.Role == llmwire.RoleTool) {
		data, err := json.Marshal(msg.Images)
		if err != nil {
			return nil, fmt.Errorf("marshal attachments: %w", err)
		}

		attachmentsJSON = data
	}

	return &transcript.Message{
		Role:                 msg.Role,
		Content:              msg.Content,
		ToolCallID:           msg.ToolCallID,
		ToolName:             msg.ToolName,
		ToolError:            msg.ToolError,
		ToolCalls:            toolCallsJSON,
		ReasoningContent:     msg.ReasoningContent,
		ReasoningRaw:         msg.ReasoningRaw,
		Attachments:          attachmentsJSON,
		CostUSD:              msg.CostUSD,
		Usage:                usageJSON,
		FinishType:           msg.FinishType,
		ProviderFinishReason: msg.ProviderFinishReason,
	}, nil
}

func (s *Session) pendingExternalCallIDs() map[string]bool {
	return s.pendingExternalCallIDsLocked(s.ms.getMessages())
}

// pendingExternalCallIDsLocked classifies unresolved calls over an already
// taken message snapshot; compaction passes its ms.mu-held transcript here
// because getMessages would re-lock and deadlock.
func (s *Session) pendingExternalCallIDsLocked(messages []llmwire.Message) map[string]bool {
	calls := unresolvedCallsMatching(messages, func(tc llmwire.ToolCall) bool {
		return s.stagedCalls[tc.ID] == tc.Name
	})

	out := make(map[string]bool, len(calls))

	for _, call := range calls {
		out[call.ID] = true
	}

	return out
}

func unresolvedCallsMatching(
	messages []llmwire.Message,
	match func(llmwire.ToolCall) bool,
) []llmwire.ToolCall {
	resolved := make(map[string]bool)

	for _, message := range messages {
		if message.Role == llmwire.RoleTool && message.ToolCallID != "" {
			resolved[message.ToolCallID] = true
		}
	}

	var result []llmwire.ToolCall

	for _, message := range messages {
		if message.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range message.ToolCalls {
			if tc.ID != "" && !resolved[tc.ID] && match(tc) {
				result = append(result, tc)
			}
		}
	}

	return result
}

func findToolCall(messages []llmwire.Message, callID string) toolCallStatus {
	var status toolCallStatus

	for _, message := range messages {
		if message.Role == llmwire.RoleAssistant {
			for _, tc := range message.ToolCalls {
				if tc.ID != callID {
					continue
				}

				if status.found {
					status.duplicate = true
				}

				status.name = tc.Name
				status.found = true
			}
		}

		if message.Role == llmwire.RoleTool && message.ToolCallID == callID {
			status.resolved = true
		}
	}

	return status
}

func (s *Session) pendingInLoopCalls() []llmwire.ToolCall {
	pending := unresolvedToolCalls(s.ms.getMessages())
	var calls []llmwire.ToolCall
	for _, message := range s.ms.getMessages() {
		for _, call := range message.ToolCalls {
			if pending[call.ID] == call.Name && s.stagedCalls[call.ID] != call.Name {
				calls = append(calls, call)
			}
		}
	}
	return calls
}

func (s *Session) unansweredWork() bool {
	messages := s.ms.getMessages()
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	return last.Role != llmwire.RoleAssistant || len(s.pendingInLoopCalls()) > 0
}
