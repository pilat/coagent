package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

const backgroundChildArgs = `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general","background":true}`

// completionProbe counts the ledger reads the completion path takes and can mask
// a delivered link as still owing — the read a sweep redelivery gets when it
// races a delivery that is committing. Only the store CAS can then reject it.
type completionProbe struct {
	subagent.Store

	mask atomic.Int64

	mu    sync.Mutex
	reads map[int64]int
}

func newCompletionProbe(inner subagent.Store) *completionProbe {
	return &completionProbe{Store: inner, reads: make(map[int64]int)}
}

func (p *completionProbe) GetLink(ctx context.Context, childID int64) (*subagent.Link, error) {
	link, err := p.Store.GetLink(ctx, childID)

	p.mu.Lock()
	p.reads[childID]++
	p.mu.Unlock()

	if err != nil || link == nil || childID != p.mask.Load() {
		return link, err
	}

	stale := *link
	stale.DeliveredAt = 0
	stale.DeliveredMsgID = 0

	return &stale, nil
}

func (p *completionProbe) readsFor(childID int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.reads[childID]
}

// casAttempt is one DeliverCompletion the daemon made, with its verdict.
type casAttempt struct {
	ActivationSeq int64
	Won           bool
}

// casProbe records every completion CAS so a test can prove the daemon reached
// the exactly-once boundary instead of silently skipping it.
type casProbe struct {
	subagent.Transactions

	mu       sync.Mutex
	attempts []casAttempt
}

func (p *casProbe) DeliverCompletion(
	ctx context.Context,
	sessionID int64,
	msgs []*sessionstore.StoredMessage,
	childID, activationSeq int64,
) ([]int64, bool, error) {
	ids, won, err := p.Transactions.DeliverCompletion(ctx, sessionID, msgs, childID, activationSeq)
	if err == nil {
		p.mu.Lock()
		p.attempts = append(p.attempts, casAttempt{ActivationSeq: activationSeq, Won: won})
		p.mu.Unlock()
	}

	return ids, won, err
}

func (p *casProbe) snapshot() []casAttempt {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]casAttempt(nil), p.attempts...)
}

// probeCAS routes the harness's completion commits through a recording store.
func probeCAS(h *subagentHarness) *casProbe {
	probe := &casProbe{Transactions: h.mgr.subagents}
	h.mgr.subagents = probe

	return probe
}

// transcriptOf reads a transcript without failing the test, since require.Never
// runs its condition on a goroutine that must not call FailNow.
func transcriptOf(h *subagentHarness, sessionID int64) []llmwire.Message {
	stored, err := h.sessStore.LoadActiveMessages(h.ctx, sessionID)
	if err != nil {
		return nil
	}

	return toDTO(stored)
}

// activationRespond drives a parent that spawns one background child and answers
// every completion it is handed; the child holds its follow-up turn on release.
func activationRespond(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(msgs, "FOLLOW_UP"):
			if release != nil {
				<-release
			}

			return &llmwire.Response{Text: "child continuation answer"}
		case hasUserContaining(msgs, "CHILD_TASK"):
			return &llmwire.Response{Text: "child finished: 42"}
		case hasToolResultFor(msgs, "subagent_event"):
			return &llmwire.Response{Text: "child completion handled"}
		case hasToolResultFor(msgs, "task"):
			return &llmwire.Response{Text: "child launched"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task", Arguments: []byte(backgroundChildArgs),
		}}}
	}
}

// A redelivery that arrives after the same activation already committed must be
// invisible: no second completion, no second model turn, and no error for the
// parent — whether the ledger read catches it or only the store CAS does.
func TestScenario_RedeliveryOfACommittedActivationIsANoOp(t *testing.T) {
	tests := []struct {
		name     string
		stale    bool
		expected []casAttempt
	}{
		{
			name:     "ledger read sees the delivery",
			expected: []casAttempt{{ActivationSeq: 1, Won: true}},
		},
		{
			name:     "stale ledger read leaves it to the store CAS",
			stale:    true,
			expected: []casAttempt{{ActivationSeq: 1, Won: true}, {ActivationSeq: 1, Won: false}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var links *completionProbe

			h := newSubagentHarnessDecorated(t, activationRespond(nil), func(inner subagent.Store) subagent.Store {
				links = newCompletionProbe(inner)
				return links
			})
			defer h.shutdown()

			cas := probeCAS(h)

			parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn a child", "fake-model", nil)
			require.NoError(t, err)

			link := h.waitForChildLink(parentID)
			h.waitForDelivery(link.ChildID)
			h.mgr.waitIdle(parentID)

			delivered, err := h.links.GetLink(h.ctx, link.ChildID)
			require.NoError(t, err)
			require.NotZero(t, delivered.DeliveredAt)
			require.Equal(t, 1, countSubagentEvents(h.parentMessages(parentID), link.ChildID))

			if tc.stale {
				links.mask.Store(link.ChildID)
			}

			readsBefore := links.readsFor(link.ChildID)

			// What sweep pass 2 enqueues when it lists a link whose delivery has not
			// been marked yet.
			require.NoError(t, h.mgr.enqueueSessionInput(h.ctx, parentID, backgroundSubagentCompletionInput{
				ChildID: link.ChildID, ActivationSeq: delivered.ActivationSeq,
			}))
			h.mgr.waitIdle(parentID)

			require.Never(t, func() bool {
				msgs := transcriptOf(h, parentID)
				return countSubagentEvents(msgs, link.ChildID) > 1 ||
					countMessageContentContaining(msgs, "child completion handled") > 1
			}, 750*time.Millisecond, 25*time.Millisecond, "a losing redelivery must change nothing")

			require.Greater(t, links.readsFor(link.ChildID), readsBefore, "the redelivery really was processed")
			assert.Equal(t, tc.expected, cas.snapshot())

			require.NoError(t, llm.ValidateToolPairing(h.parentMessages(parentID)))

			rec, err := h.sessStore.GetSession(h.ctx, parentID)
			require.NoError(t, err)
			assert.NotEqual(t, sessionstore.SessionStatusError, rec.Status, "a lost CAS must not error the parent")
		})
	}
}

// Delivery is the ordering barrier between activations: a completion owed by the
// activation that died cannot be spent on the activation a follow-up opened.
func TestScenario_StaleActivationCompletionCannotFillTheNewActivation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "activation.db")
	release := make(chan struct{})
	released := false

	defer func() {
		if !released {
			close(release)
		}
	}()

	first := newSubagentHarnessOnDB(t, dbPath, activationRespond(release), nil)

	parentID, err := first.mgr.Send(first.ctx, first.projectID, "spawn a child", "fake-model", nil)
	require.NoError(t, err)

	link := first.waitForChildLink(parentID)
	first.waitForDelivery(link.ChildID)
	first.mgr.waitIdle(parentID)
	first.shutdown()

	var links *completionProbe

	second := newSubagentHarnessOnDB(t, dbPath, activationRespond(release), func(inner subagent.Store) subagent.Store {
		links = newCompletionProbe(inner)
		return links
	})
	defer second.shutdown()

	cas := probeCAS(second)

	require.NoError(t, second.mgr.Start(second.ctx))
	require.NoError(t, second.mgr.SendToChild(second.ctx, link.ChildID, "FOLLOW_UP one more thing"))

	second.waitUntil("the follow-up opened a second activation", func() bool {
		current, linkErr := second.links.GetLink(second.ctx, link.ChildID)
		return linkErr == nil && current != nil &&
			current.ActivationSeq == 2 && current.DeliveredAt == 0 && !current.Terminal()
	})

	readsBefore := links.readsFor(link.ChildID)

	// The completion the dead daemon owed for activation 1 arrives late.
	require.NoError(t, second.mgr.enqueueSessionInput(second.ctx, parentID, backgroundSubagentCompletionInput{
		ChildID: link.ChildID, ActivationSeq: 1,
	}))
	second.mgr.waitIdle(parentID)

	require.Never(t, func() bool {
		return countSubagentEvents(transcriptOf(second, parentID), link.ChildID) > 1
	}, 500*time.Millisecond, 25*time.Millisecond, "the stale activation adds nothing to the transcript")

	require.Greater(t, links.readsFor(link.ChildID), readsBefore, "the stale completion really was processed")
	assert.Empty(t, cas.snapshot(), "a stale activation never reaches the delivery CAS")

	owing, err := second.links.GetLink(second.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Zero(t, owing.DeliveredAt, "activation 2 is still owed its own completion")

	close(release)

	released = true

	second.waitUntil("activation 2 delivered", func() bool {
		current, linkErr := second.links.GetLink(second.ctx, link.ChildID)
		return linkErr == nil && current != nil && current.DeliveredAt != 0 && current.ActivationSeq == 2
	})
	second.mgr.waitIdle(parentID)

	assert.Equal(t, []casAttempt{{ActivationSeq: 2, Won: true}}, cas.snapshot())

	final := second.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(final), "no crossed pairing between activations")
	assert.Equal(t, 2, countSubagentEvents(final, link.ChildID), "one completion per activation, in order")
}
