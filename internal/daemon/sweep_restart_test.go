package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
)

// errDeliveryCrash stands in for the process dying between a child's terminal
// mark and the commit of its completion into the parent transcript.
var errDeliveryCrash = errors.New("daemon died before the completion was committed")

// crashGateLinkStore rejects every link read taken while a completion is owed but
// not yet committed, so a daemon can be torn down inside that exact window.
type crashGateLinkStore struct {
	LinkStore

	once     sync.Once
	rejected chan struct{}
}

func newCrashGate(inner LinkStore) *crashGateLinkStore {
	return &crashGateLinkStore{LinkStore: inner, rejected: make(chan struct{})}
}

func (g *crashGateLinkStore) GetLink(ctx context.Context, childID int64) (*SubagentLink, error) {
	link, err := g.LinkStore.GetLink(ctx, childID)
	if err != nil || link == nil || !link.Terminal() || link.DeliveredAt != 0 {
		return link, err
	}

	g.once.Do(func() { close(g.rejected) })

	return nil, errDeliveryCrash
}

// crashWindowRespond drives a parent that spawns exactly one child and answers
// once its completion arrives, in either delivery shape.
func crashWindowRespond(background bool) func(string, []llmwire.Message) *llmwire.Response {
	args := `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`
	if background {
		args = `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general","background":true}`
	}

	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(msgs, "CHILD_TASK"):
			return &llmwire.Response{Text: "child finished: 42"}
		case hasToolResultFor(msgs, "subagent_event"):
			return &llmwire.Response{Text: "parent got the child result"}
		case hasToolResultFor(msgs, "task") && background:
			return &llmwire.Response{Text: "child launched"}
		case hasToolResultFor(msgs, "task"):
			return &llmwire.Response{Text: "parent got the child result"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task", Arguments: []byte(args),
		}}}
	}
}

// A completion the ledger owes must survive the process that owed it: daemon A
// dies with the child terminal and the parent transcript empty, and daemon B's
// sweep commits it exactly once — never twice, never zero times.
func TestScenario_CrashBetweenFinalizationAndDeliveryRedeliversExactlyOnce(t *testing.T) {
	tests := []struct {
		name        string
		background  bool
		completions func(msgs []llmwire.Message, childID int64) int
	}{
		{
			name: "blocking child fills its task call",
			completions: func(msgs []llmwire.Message, _ int64) int {
				return countToolResultsFor(msgs, "task")
			},
		},
		{
			name:        "background child arrives as a subagent event",
			background:  true,
			completions: countSubagentEvents,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "crash.db")

			var gate *crashGateLinkStore

			first := newSubagentHarnessOnDB(
				t, dbPath, crashWindowRespond(tc.background),
				func(inner LinkStore) LinkStore {
					gate = newCrashGate(inner)
					return gate
				},
			)

			parentID, err := first.mgr.Send(first.ctx, first.projectID, "spawn a child", "fake-model", nil)
			require.NoError(t, err)

			link := first.waitForChildLink(parentID)

			select {
			case <-gate.rejected:
			case <-time.After(5 * time.Second):
				t.Fatal("daemon A never reached the owed-completion window")
			}

			first.shutdown()

			second := newSubagentHarnessOnDB(t, dbPath, crashWindowRespond(tc.background), nil)
			defer second.shutdown()

			owed, err := second.links.GetLink(second.ctx, link.ChildID)
			require.NoError(t, err)
			require.NotNil(t, owed)
			require.True(t, owed.Terminal(), "the child was finalized before the crash")
			require.Zero(t, owed.DeliveredAt, "but its completion never reached the parent")
			require.Zero(t, tc.completions(second.parentMessages(parentID), link.ChildID))

			require.NoError(t, second.mgr.Start(second.ctx))

			second.waitUntil("sweep redelivered the owed completion", func() bool {
				current, linkErr := second.links.GetLink(second.ctx, link.ChildID)
				return linkErr == nil && current != nil && current.DeliveredAt != 0
			})
			second.mgr.waitIdle(parentID)

			msgs := second.parentMessages(parentID)
			require.NoError(t, llm.ValidateToolPairing(msgs), "the recovered transcript must stay provider-valid")
			assert.Equal(t, 1, tc.completions(msgs, link.ChildID), "the redelivery commits exactly one completion")
			assert.Equal(t, "parent got the child result", lastAssistantTextDTO(msgs),
				"the parent resumed and consumed the recovered completion")
		})
	}
}

// /stop is an explicit park, not an interrupted run: a stopped link carries no
// owed completion, so no sweep may resume the child or deliver anything for it.
func TestScenario_StoppedChildSurvivesARestartWithoutResurrection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped.db")
	release := make(chan struct{})

	defer close(release)

	held := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-release
			return &llmwire.Response{Text: "child must never finish"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "child launched"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task",
			Arguments: []byte(
				`{"prompt":"CHILD_TASK hang","description":"c","subagent_type":"general","background":true}`,
			),
		}}}
	}

	// Daemon B's child would answer immediately, so a wrongly resumed child shows
	// up as a real delivery rather than as another hang.
	eager := crashWindowRespond(true)

	first := newSubagentHarnessOnDB(t, dbPath, held, nil)

	parentID, err := first.mgr.Send(first.ctx, first.projectID, "spawn a child", "fake-model", nil)
	require.NoError(t, err)

	link := first.waitForChildLink(parentID)
	first.waitUntil("child loop is live", func() bool { return first.mgr.HasActiveLoop(link.ChildID) })

	require.NoError(t, first.mgr.Stop(first.ctx, link.ChildID, 0))

	parked, err := first.links.GetLink(first.ctx, link.ChildID)
	require.NoError(t, err)
	require.Equal(t, LinkStateStopped, parked.State)

	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, eager, nil)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.mgr.sweep(second.ctx)

	require.Never(t, func() bool {
		current, linkErr := second.links.GetLink(second.ctx, link.ChildID)
		return linkErr == nil && current != nil &&
			(current.State != LinkStateStopped || current.DeliveredAt != 0)
	}, 500*time.Millisecond, 25*time.Millisecond, "a stopped child stays parked across a restart")

	assert.Zero(t, countSubagentEvents(second.parentMessages(parentID), link.ChildID),
		"a stopped child owes the parent nothing")
	assert.False(t, second.mgr.HasActiveLoop(link.ChildID), "the sweep must not start a stopped child")
}
