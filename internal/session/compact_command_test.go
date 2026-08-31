package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// compactCommandRunner wires a runner over a durable inbox holding one input.
func compactCommandRunner(s *svc, content string, notes *[]string) (*loopRunner, *loopInputBoundary) {
	boundary := &loopInputBoundary{
		agent: s,
		input: &PendingInput{ID: 1, Content: content, ReceivedAt: time.Now()},
	}
	s.boundary = boundary

	return contextEventRunner(s, notes), boundary
}

// pendingCallTranscript is a transcript whose last assistant turn holds one
// unanswered external call.
func pendingCallTranscript(callID, toolName string) []llmwire.Message {
	return []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		{
			Role:      llmwire.RoleAssistant,
			Content:   "waiting",
			ToolCalls: []llmwire.ToolCall{{ID: callID, Name: toolName}},
		},
	}
}

func TestBoundaryCommand_IgnoresOtherPrompts(t *testing.T) {
	s := newCompactionTestSvc(&compactionMockLLM{})

	var notes []string
	r, b := compactCommandRunner(s, "do the thing", &notes)

	outcome, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)
	assert.Equal(t, commandNotRecognized, outcome)

	b.input = &PendingInput{ID: 2, Content: "/compactify now"}
	outcome, err = r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)
	assert.Equal(t, commandNotRecognized, outcome, "/compact must be a whole command, not a prefix")
}

// /compact no longer compacts from the side: it raises the flag the loop's one
// sanctioned compaction point reads.
func TestSlashCompact_RaisesTheFlagAndCompactsAtTheLoopPoint(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(loopRounds(10, 4000))

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	outcome, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	assert.Equal(t, commandDeferred, outcome)
	assert.NotNil(t, b.input, "the request remains pending until its terminal outcome")
	assert.True(t, s.compactionRequested())
	assert.Zero(t, llm.callCount, "nothing is summarized from the command handler")

	r.applyContextEvents(t.Context())

	assert.Positive(t, llm.callCount)
	assert.True(t, notesContain(notes, "✅ Context compacted"))
	assert.Empty(t, s.compactionFocus, "focus is one-shot")

	for _, p := range llm.prompts {
		assert.NotContains(t, p, "Priority for this summary:", "bare /compact carries no focus section")
	}
}

func TestSlashCompact_EmptySessionRepliesNothingToCompact(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	_, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	r.applyContextEvents(t.Context())

	assert.Zero(t, llm.callCount)
	assert.True(t, notesContain(notes, "Nothing to compact"))
}

func TestSlashCompact_FocusThreadsIntoTheSummarizationPrompt(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(loopRounds(10, 4000))

	var notes []string
	r, b := compactCommandRunner(s, compactCommand+" focus on the auth bug", &notes)

	_, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)
	assert.Equal(t, "focus on the auth bug", s.compactionFocus)

	r.applyContextEvents(t.Context())

	require.NotEmpty(t, llm.prompts)
	assert.Contains(t, llm.prompts[0], "Priority for this summary: focus on the auth bug")
	assert.Empty(t, s.compactionFocus)
}

func TestSlashCompact_CompactionFailureIsReported(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	store := &compactionRecordingStore{nextID: 1, markCompactedErr: errors.New("write conflict")}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)
	s.ms.setMessages(loopRounds(10, 4000))

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	_, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	r.applyContextEvents(t.Context())

	assert.True(t, notesContain(notes, "❌ Compaction failed"))
	assert.False(t, notesContain(notes, "✅ Context compacted"))
	assert.Empty(t, s.compactionFocus, "focus is cleared even when compaction fails")
}

// Behind a blocking call the request stays durable. An in-memory flag would die
// with the svc the resume rebuilds; the inbox survives even a daemon restart.
func TestSlashCompact_DefersBehindANonSleepPendingCall(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{"t1": tool.IDTask}
	s.ms.setMessages(pendingCallTranscript("t1", tool.IDTask))

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	outcome, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	assert.Equal(t, commandDeferred, outcome)
	assert.NotNil(t, b.input, "the request stays in the durable inbox")
	assert.False(t, s.compactionRequested(), "no flag is raised while the call is out")
	assert.Equal(t, 1, countNotes(notes, compactionDeferredNotice))

	// One notice per deferral episode, however many times the drain comes round.
	_, err = r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)
	assert.Equal(t, 1, countNotes(notes, compactionDeferredNotice))
}

// Sleep yields to /compact exactly as it yields to any user message: deferring
// here would mute the messages queued behind it and spin the runner, which
// considers "input + sleep only" runnable.
func TestSlashCompact_InterruptsSleepInsteadOfDeferring(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{"s1": tool.IDSleep}
	s.ms.setMessages(pendingCallTranscript("s1", tool.IDSleep))

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	outcome, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	assert.Equal(t, commandDeferred, outcome)
	assert.True(t, s.compactionRequested())
	assert.False(t, s.HasPendingExternalCall(), "the sleep was resolved, not left hanging")
	assert.Zero(t, countNotes(notes, compactionDeferredNotice))

	msgs := s.ms.getMessages()
	last := msgs[len(msgs)-1]
	assert.Equal(t, llmwire.RoleTool, last.Role)
	assert.Equal(t, sleepInterruptedMessage, last.Content)
}

// A compaction requested while nothing is outstanding must still run when the
// loop is about to unwind on a handled control command.
func TestRunLoopRunsADeferredCompactionBeforeReturning(t *testing.T) {
	agent := newTestAgent()
	agent.llmClient = summarizingLLM()
	agent.maxIterations = 5
	agent.ms.setMessages(loopRounds(10, 4000))

	notifier := &loopNotifier{}
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: compactCommand, ReceivedAt: time.Now()},
	}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Equal(t, 1, notifier.countWith("✅ Context compacted"))
	assert.True(t, hasSummaryRow(agent.ms.getMessages()))
}

// The gate is strict: with an external call outstanding the early return must not
// run compaction, or compact() would meet its own guard.
func TestRunLoopDoesNotCompactOnASuspendPath(t *testing.T) {
	agent := newTestAgent()
	agent.llmClient = summarizingLLM()
	agent.maxIterations = 5
	agent.stagedCalls = map[string]string{"t1": tool.IDTask}
	agent.ms.setMessages(pendingCallTranscript("t1", tool.IDTask))
	agent.RequestCompaction()

	notifier := &loopNotifier{}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.True(t, result.Suspended)
	assert.Zero(t, notifier.countWith("Compacting context"))
	assert.False(t, hasSummaryRow(agent.ms.getMessages()))
}

func hasSummaryRow(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if isMarkedSummary(m.Content) {
			return true
		}
	}

	return false
}

// A crash between recording an assistant turn and executing its tools leaves
// work owing. On resume the loop settles that work first and only then honours
// the queued /compact — otherwise compaction would delete a call nobody ran.
func TestRunLoopExecutesOwedToolsBeforeAQueuedCompaction(t *testing.T) {
	executed := make(chan string, 4)
	agent := newTestAgent(&recordingTool{id: "read", result: "file body", seen: executed})
	agent.llmClient = summarizingLLM()
	agent.maxIterations = 5
	agent.ms.setMessages(append(loopRounds(4, 4000), llmwire.Message{
		Role:      llmwire.RoleAssistant,
		Content:   "reading",
		ToolCalls: []llmwire.ToolCall{{ID: "owed", Name: "read", Arguments: []byte(`{}`)}},
	}))

	notifier := &loopNotifier{}
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: compactCommand, ReceivedAt: time.Now()},
	}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	require.Len(t, executed, 1, "the owed tool ran exactly once")
	assert.Equal(t, 1, notifier.countWith("✅ Context compacted"))
	assert.True(t, hasSummaryRow(agent.ms.getMessages()))
}

// The invariant, over every arrangement of what may be outstanding: compaction
// never runs while a call is pending. Abandoned tool_uses are deliberately not
// covered — they are legal and the rebuild destroys them with everything else.
func TestCompactionNeverRunsWithPendingCalls(t *testing.T) {
	tests := []struct {
		name        string
		transcript  []llmwire.Message
		staged      map[string]string
		explicit    bool
		wantRefused bool
	}{
		{
			name:        "external call outstanding, explicit request",
			transcript:  pendingCallTranscript("t1", tool.IDTask),
			staged:      map[string]string{"t1": tool.IDTask},
			explicit:    true,
			wantRefused: true,
		},
		{
			name:        "external call outstanding, threshold request",
			transcript:  pendingCallTranscript("t1", tool.IDSleep),
			staged:      map[string]string{"t1": tool.IDSleep},
			wantRefused: true,
		},
		{
			name:        "ordinary tool work owed",
			transcript:  pendingCallTranscript("c1", "read"),
			explicit:    true,
			wantRefused: true,
		},
		{
			name: "abandoned tool_use behind a newer user turn",
			transcript: append(
				pendingCallTranscript("c1", "read"),
				compactionUserMessage("never mind, do this instead"),
			),
			explicit:    true,
			wantRefused: false,
		},
		{
			name: "everything settled",
			transcript: append(
				pendingCallTranscript("c1", "read"),
				llmwire.Message{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "body"},
				compactionUserMessage("carry on"),
			),
			explicit:    true,
			wantRefused: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			llm := &compactionMockLLM{
				response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
				contextWindow: 200000,
			}
			s := newCompactionTestSvc(llm)
			s.stagedCalls = tc.staged
			s.ms.setMessages(tc.transcript)

			if tc.explicit {
				s.RequestCompaction()
			}

			var notes []string
			contextEventRunner(s, &notes).applyContextEvents(t.Context())

			if tc.wantRefused {
				assert.Zero(t, llm.callCount, "no summarization while a call is pending")
				assert.False(t, hasSummaryRow(s.ms.getMessages()))

				return
			}

			assert.Equal(t, 1, llm.callCount)
			assert.True(t, hasSummaryRow(s.ms.getMessages()))
		})
	}
}

// recordingTool reports each invocation on a channel.
type recordingTool struct {
	id     string
	result string
	seen   chan string
}

func (r *recordingTool) ID() string                  { return r.id }
func (r *recordingTool) Description() string         { return "records invocations" }
func (r *recordingTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (r *recordingTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	r.seen <- r.id
	return &tool.Result{Output: r.result}, nil
}
