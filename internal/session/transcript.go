package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// compactionFraction is the share of the context window at which auto-compaction
// fires. The /status 🔴 band reuses it as its cutoff.
const compactionFraction = llmwire.ContextInputFraction

// imageTokenCeiling bounds the estimator's per-image charge when dimensions
// are absent.
const imageTokenCeiling = 8192

// Image byte/count pressure marks (D4/D5). Bytes are base64 wire sizes: the
// observed incident wall sits at 35.8/38.8 MB, one observation of a
// load-dependent time wall, so the trigger keeps a ×3 margin. Counts use
// Bedrock's published per-request limit. Low-water is half of high-water on
// both axes, giving the boundary hysteresis of roughly one image batch.
const (
	imageBytesHighWater = 12 << 20 // 12 MB base64 — compaction trigger
	imageBytesLowWater  = 6 << 20  // 6 MB base64 — tail ceiling
	imageCountHighWater = 20       // compaction trigger
	imageCountLowWater  = 10       // tail ceiling
	// imagePatchQuantumPx is the measured token quantum per pixel patch,
	// exact for the incident model (D4/T5).
	imagePatchQuantumPx = 28
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

// contextBaseline is the last real measurement: the provider's own cache-
// inclusive PromptTokens and how many transcript messages it covered.
type contextBaseline struct {
	promptTokens int
	messageCount int
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

// UnresolvedStoredCalls uses the same call resolution rules as the live loop.
func UnresolvedStoredCalls(messages []*transcript.Message) ([]PendingToolCall, error) {
	wire := make([]llmwire.Message, 0, len(messages))
	for _, message := range messages {
		row := llmwire.Message{Role: message.Role, ToolCallID: message.ToolCallID}
		if len(message.ToolCalls) > 0 {
			if err := json.Unmarshal(message.ToolCalls, &row.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode tool calls of message %d: %w", message.ID, err)
			}
		}

		wire = append(wire, row)
	}

	calls := unresolvedCallsMatching(wire, func(llmwire.ToolCall) bool { return true })

	result := make([]PendingToolCall, 0, len(calls))
	for _, call := range calls {
		result = append(result, PendingToolCall{ID: call.ID, Name: call.Name})
	}

	return result, nil
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

func (ms *messageStore) getMessages() []llmwire.Message {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	result := make([]llmwire.Message, len(ms.messages))
	copy(result, ms.messages)

	return result
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

	if len(messages) == 1 && strings.HasPrefix(messages[0].Content, agentsMDMessagePrefix) {
		return false
	}

	last := messages[len(messages)-1]

	return last.Role != llmwire.RoleAssistant || len(s.pendingInLoopCalls()) > 0
}

// repairTranscript ensures tool calls and results are properly paired.
// It performs four repairs matching OpenClaw's repairToolUseResultPairing:
//  1. Drops orphaned tool results (no matching assistant tool call)
//  2. Inserts synthetic error results for tool calls with no result
//  3. Reorders tool results to immediately follow their assistant message
//  4. Drops duplicate results for the same tool call ID
func repairTranscript(messages []llmwire.Message) []llmwire.Message {
	return repairTranscriptExcluding(messages, nil)
}

// repairTranscriptExcluding is repairTranscript that never fabricates a result
// for a tool_call in pendingCallIDs — those are genuinely-pending external calls
// (sleep, blocking task) awaiting an out-of-band outcome; stubbing them would
// corrupt the resume. The loop never reaches the LLM with such a call pending
// (handlePreviousResult returns first), so this is a defensive guard.
func repairTranscriptExcluding(messages []llmwire.Message, pendingCallIDs map[string]bool) []llmwire.Message {
	if len(messages) == 0 {
		return messages
	}

	allToolCallIDs := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role == llmwire.RoleAssistant {
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					allToolCallIDs[tc.ID] = true
				}
			}
		}
	}

	// Index tool results by their ToolCallID for reordering
	resultsByCallID := make(map[string]llmwire.Message)
	seenResultIDs := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role == llmwire.RoleTool && msg.ToolCallID != "" {
			if seenResultIDs[msg.ToolCallID] {
				continue // duplicate — keep first only
			}

			if allToolCallIDs[msg.ToolCallID] {
				resultsByCallID[msg.ToolCallID] = msg
				seenResultIDs[msg.ToolCallID] = true
			}
			// else: orphaned — will be dropped
		}
	}

	// Rebuild: walk messages, emit assistant + ordered results, skip bare tool messages
	result := make([]llmwire.Message, 0, len(messages))
	emittedResults := make(map[string]bool)

	for _, msg := range messages {
		switch {
		case msg.Role == llmwire.RoleAssistant && len(msg.ToolCalls) > 0:
			result = append(result, msg)
			emitToolResults(&result, msg.ToolCalls, resultsByCallID, emittedResults, pendingCallIDs)

		case msg.Role == llmwire.RoleTool && msg.ToolCallID != "":
			// Skip — already handled above (reordered or dropped)
			continue

		case msg.Role == llmwire.RoleTool && msg.ToolCallID == "":
			// Legacy tool results with no ID — preserve as-is
			result = append(result, msg)

		default:
			// User messages, plain assistant messages — keep as-is
			result = append(result, msg)
		}
	}

	return result
}

// emitToolResults appends ordered tool results (or synthetic error stubs) for an assistant message's tool calls.
func emitToolResults(
	result *[]llmwire.Message,
	toolCalls []llmwire.ToolCall,
	resultsByCallID map[string]llmwire.Message,
	emittedResults map[string]bool,
	pendingCallIDs map[string]bool,
) {
	incomplete := hasIncompleteToolCalls(toolCalls)

	for _, tc := range toolCalls {
		if tc.ID == "" || emittedResults[tc.ID] {
			continue
		}

		emittedResults[tc.ID] = true

		if tr, ok := resultsByCallID[tc.ID]; ok {
			*result = append(*result, tr)
			continue
		}

		// A genuinely-pending external call must not be stubbed — leave it
		// unmatched (the loop won't send this transcript while it's pending).
		if pendingCallIDs[tc.ID] {
			continue
		}

		if incomplete {
			continue
		}

		*result = append(*result, llmwire.Message{
			Role:       llmwire.RoleTool,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    fmt.Sprintf("[transcript repair] missing tool result for %s (id: %s)", tc.Name, tc.ID),
		})
	}
}

// hasIncompleteToolCalls returns true if any tool call looks incomplete
// (missing ID or name), indicating the assistant message was likely
// aborted or errored mid-generation.
func hasIncompleteToolCalls(calls []llmwire.ToolCall) bool {
	for _, tc := range calls {
		if tc.ID == "" || tc.Name == "" {
			return true
		}
	}

	return false
}

// estimateText is the len/4 rule applied to raw text.
func estimateText(text string) int {
	return len(text) / 4
}

// estimateTokens is a rough estimate of the conversation's own size — never
// persisted, never truth. Tool-call Arguments count: file bodies live there.
func estimateTokens(messages []llmwire.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Content) / 4
		total += len(msg.ReasoningContent) / 4

		for _, tc := range msg.ToolCalls {
			total += len(tc.Arguments) / 4
		}

		for _, ref := range msg.Images {
			total += imageTokenEstimate(ref)
		}
	}

	return total
}

// imageTokenEstimate charges the measured 28-pixel patch quantum — exact for
// the incident model and the best proxy for every other cataloged vision
// model; file size, the previous basis, provably does not affect image cost.
func imageTokenEstimate(ref llmwire.ImageRef) int {
	if ref.Width > 0 && ref.Height > 0 {
		q := imagePatchQuantumPx

		return ((ref.Width + q - 1) / q) * ((ref.Height + q - 1) / q)
	}

	// Without dimensions, size capped: providers downscale oversized images,
	// so raw Size/4 would over-count ordinary photos.
	return min(int(ref.Size)/4, imageTokenCeiling)
}

// estimateSchemas estimates the tool inventory a request carries.
func estimateSchemas(schemas []llmwire.ToolSchema) int {
	total := 0

	for _, schema := range schemas {
		total += estimateText(schema.Name) + estimateText(schema.Description) + len(schema.Parameters)/4
	}

	return total
}

// compactionCutoff is the projected size above which compaction fires.
func compactionCutoff(window int) int {
	return int(compactionFraction * float64(window))
}

// projectContextSize is the last measured number plus a len/4 estimate of
// everything appended since. A baseline the transcript shrank under is discarded.
func projectContextSize(messages []llmwire.Message, base *contextBaseline, overhead int) int {
	if base != nil && base.messageCount <= len(messages) {
		return base.promptTokens + estimateTokens(messages[base.messageCount:])
	}

	return estimateTokens(messages) + overhead
}

// requestOverhead is what a request carries besides the conversation. Only the
// unmeasured projection needs it — a measured baseline already includes it.
func (s *Session) requestOverhead() int {
	return estimateText(s.prompt.SystemPrompt()) + estimateSchemas(tool.ToSchemas(s.registry.List()))
}

// projectContextSize reports the projection and whether it is a pure estimate
// (no provider measurement backing it).
func (s *Session) projectContextSize() (int, bool) {
	messages := s.ms.getMessages()
	overhead := s.requestOverhead()
	base := s.loadContextBaseline()

	return projectContextSize(messages, base, overhead), base == nil
}

func (s *Session) loadContextBaseline() *contextBaseline {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.baseline
}

// modelGeneration identifies the model a request is about to go out under.
func (s *Session) modelGeneration() uint64 {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.modelEpoch
}

// storeContextBaseline installs the measurement in memory when it describes the
// current model generation, returning the model to persist it under.
func (s *Session) storeContextBaseline(promptTokens, sentCount int, generation uint64) (string, bool) {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	if s.modelEpoch != generation {
		return "", false
	}

	s.baseline = &contextBaseline{promptTokens: promptTokens, messageCount: sentCount}

	return s.model, true
}

// installPersistedBaseline adopts the last measurement across a restart. It is
// discarded when the session's current model differs — a measurement describes
// one model's window and tokenizer, the same rule the in-memory modelEpoch
// encodes for mid-flight switches.
func (s *Session) installPersistedBaseline(b *sessionstore.ContextBaseline) {
	if b == nil || b.Model != s.model || b.PromptTokens <= 0 {
		return
	}

	s.baseline = &contextBaseline{promptTokens: b.PromptTokens, messageCount: b.MessageCount}
}

// resetContextBaseline drops back to pure estimation.
func (s *Session) resetContextBaseline() {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	s.baseline = nil
}

// hasCompactionCandidate reports whether the raw range can yield a checkpoint
// split at all. The tail is never empty (D3), so a transcript whose only legal
// group would have to stay verbatim has nothing to summarize — the automatic
// path must not announce an attempt it can never make. The same head-fit bound
// compactLocked applies is included, so the pre-check and the authoritative
// re-selection inside compact() agree.
func (s *Session) hasCompactionCandidate(window int) bool {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	headerSize := compactionHeaderSize(s.ms.messages)

	cp := parseCheckpointPrefix(s.ms.messages, headerSize)
	if cp.rawStart >= len(s.ms.messages) {
		return false
	}

	baseEstimate := s.summarizerBaseEstimateLocked()
	minTail := minTailTokens(s.ms.messages, cp.rawStart, window)

	for _, fraction := range []float64{compactionRequestFraction, llmwire.ContextInputFraction} {
		for _, limit := range tailLevels() {
			args := selectTailSplitArgs{
				messages: s.ms.messages, base: cp.rawStart, minTail: minTail,
				requestBaseEstimate: baseEstimate, window: window, limit: limit, inputFraction: fraction,
			}
			if _, ok := selectTailSplit(args); ok {
				return true
			}
		}
	}

	return false
}

// shouldCompact reports whether the projected request size exceeds
// compactionFraction of the window, or image pressure breaches a high-water
// mark (D1/D5): a byte wall the token projection cannot see.
func (s *Session) shouldCompact(window int) bool {
	size, _ := s.projectContextSize()
	if size > compactionCutoff(window) {
		return true
	}

	totalBytes, count := imagePressure(s.ms.getMessages())

	return totalBytes > imageBytesHighWater || count > imageCountHighWater
}

// imageBase64Bytes is the wire size of one stored attachment, derived from its
// recorded size. No disk access keeps the projection deterministic, and a file
// that vanished mid-session still counts: it cannot free budget or make the
// boundary retreat.
func imageBase64Bytes(size int64) int64 {
	return (size + 2) / 3 * 4
}

// imagePressure sums the base64 wire bytes and the count of every attachment
// the transcript would carry, including refs a driver would degrade —
// conservatism is the point. Both the compaction trigger and the tail ceiling
// read the same projection.
func imagePressure(messages []llmwire.Message) (int64, int) {
	var totalBytes int64

	count := 0

	for _, msg := range messages {
		for _, ref := range msg.Images {
			totalBytes += imageBase64Bytes(ref.Size)
			count++
		}
	}

	return totalBytes, count
}
