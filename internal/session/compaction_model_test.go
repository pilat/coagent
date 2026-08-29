package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// compactionCommand is deliberately smaller than the implementation API: these
// are the externally meaningful transitions whose interleaving decides whether
// compaction may run.
type compactionCommand byte

const (
	cmdQueueCompact compactionCommand = iota
	cmdStartExternalCall
	cmdDeliverExternalResult
	cmdEmitToolCall
	cmdExecuteTools
	cmdSupersedeWithUserTurn
	cmdRunLoopPoint
)

var compactionAlphabet = []compactionCommand{
	cmdQueueCompact,
	cmdStartExternalCall,
	cmdDeliverExternalResult,
	cmdEmitToolCall,
	cmdExecuteTools,
	cmdSupersedeWithUserTurn,
	cmdRunLoopPoint,
}

// compactionProtocolModel is the reference: what a reader of the plan expects,
// independent of how the session implements it.
type compactionProtocolModel struct {
	queuedCompact    bool
	externalPending  bool
	workPending      bool
	freshContent     bool
	compactionsRun   int
	compactionsHoped int
}

func (m *compactionProtocolModel) apply(command compactionCommand) {
	switch command {
	case cmdQueueCompact:
		m.queuedCompact = true
	case cmdStartExternalCall:
		if !m.externalPending && !m.workPending {
			m.externalPending = true
			m.freshContent = true
		}
	case cmdDeliverExternalResult:
		if m.externalPending {
			m.externalPending = false
			m.freshContent = true
		}
	case cmdEmitToolCall:
		if !m.externalPending && !m.workPending {
			m.workPending = true
			m.freshContent = true
		}
	case cmdExecuteTools:
		if m.workPending {
			m.workPending = false
			m.freshContent = true
		}
	case cmdSupersedeWithUserTurn:
		// A later user turn abandons a dangling ordinary call; an external call
		// keeps its producer and stays pending.
		m.workPending = false
		m.freshContent = true
	case cmdRunLoopPoint:
		if !m.queuedCompact || m.externalPending || m.workPending {
			return
		}

		m.queuedCompact = false

		// A transcript holding only the previous compaction's own output has
		// nothing left to summarize.
		if m.freshContent {
			m.compactionsHoped++
			m.freshContent = false
		}
	}
}

// TestCompactionProtocolModel drives every sequence of commands up to a bounded
// length through both the reference model and the real session, asserting the
// invariant the plan states: compaction never runs while a call is pending, and
// a queued /compact is never silently dropped.
func TestCompactionProtocolModel(t *testing.T) {
	const depth = 4

	sequences := compactionSequences(depth)
	require.NotEmpty(t, sequences)

	for _, sequence := range sequences {
		t.Run(sequenceName(sequence), func(t *testing.T) {
			runCompactionSequence(t, sequence)
		})
	}
}

func runCompactionSequence(t *testing.T, sequence []compactionCommand) {
	t.Helper()

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{}
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("seed", "work"),
		compactionToolResult("seed", "result"),
	})

	// The seeded round is content the first compaction can summarize.
	model := &compactionProtocolModel{freshContent: true}
	next := 0

	var notes []string

	runner := contextEventRunner(s, &notes)

	for step, command := range sequence {
		next++

		before := llm.callCount
		pendingBefore := model.externalPending || model.workPending

		applyToSession(t, s, runner, command, next)
		model.apply(command)

		if llm.callCount > before {
			model.compactionsRun++

			assert.False(t, pendingBefore,
				"step %d (%v): compaction ran while a call was pending", step, command)
		}

		assert.Equal(t, model.externalPending, s.HasPendingExternalCall(),
			"step %d (%v): external-pending diverged from the model", step, command)
		assert.Equal(t, model.workPending, s.HasPendingWork(),
			"step %d (%v): pending work diverged from the model", step, command)
	}

	assert.Equal(t, model.compactionsHoped, model.compactionsRun,
		"every compaction the model expects must have happened, and no others")
	assert.Equal(t, model.queuedCompact, s.compactionRequested(),
		"a queued /compact is neither dropped nor invented")
}

func applyToSession(t *testing.T, s *svc, runner *loopRunner, command compactionCommand, seq int) {
	t.Helper()

	switch command {
	case cmdQueueCompact:
		s.RequestCompaction()
	case cmdStartExternalCall:
		if s.HasPendingExternalCall() || s.HasPendingWork() {
			return
		}

		id := fmt.Sprintf("ext-%d", seq)
		s.stagedCalls[id] = tool.IDTask
		appendMessages(t, s, llmwire.Message{
			Role:      llmwire.RoleAssistant,
			Content:   "spawning",
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: tool.IDTask}},
		})
	case cmdDeliverExternalResult:
		for _, call := range s.PendingExternalCalls() {
			appendMessages(t, s, llmwire.Message{
				Role: llmwire.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "child done",
			})
		}
	case cmdEmitToolCall:
		if s.HasPendingExternalCall() || s.HasPendingWork() {
			return
		}

		appendMessages(t, s, compactionAssistantCall(fmt.Sprintf("work-%d", seq), "reading"))
	case cmdExecuteTools:
		for id := range unresolvedToolCalls(s.ms.getMessages()) {
			if s.stagedCalls[id] != "" {
				continue
			}

			appendMessages(t, s, llmwire.Message{
				Role: llmwire.RoleTool, ToolCallID: id, ToolName: "read", Content: "body",
			})
		}
	case cmdSupersedeWithUserTurn:
		appendMessages(t, s, compactionUserMessage(fmt.Sprintf("new instruction %d", seq)))
	case cmdRunLoopPoint:
		// Reaching the loop's single compaction point says nothing about safety —
		// deciding that is the production code's job, which is what this exercises.
		runner.applyContextEvents(t.Context())
	}
}

func appendMessages(t *testing.T, s *svc, msgs ...llmwire.Message) {
	t.Helper()

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.ms.messages = append(s.ms.messages, msgs...)
}

// compactionSequences enumerates every command sequence of exactly depth steps.
func compactionSequences(depth int) [][]compactionCommand {
	sequences := [][]compactionCommand{{}}

	for range depth {
		next := make([][]compactionCommand, 0, len(sequences)*len(compactionAlphabet))

		for _, prefix := range sequences {
			for _, command := range compactionAlphabet {
				extended := make([]compactionCommand, len(prefix), len(prefix)+1)
				copy(extended, prefix)
				next = append(next, append(extended, command))
			}
		}

		sequences = next
	}

	return sequences
}

func sequenceName(sequence []compactionCommand) string {
	var name strings.Builder

	for _, command := range sequence {
		name.WriteRune('A' + rune(command))
	}

	return name.String()
}
