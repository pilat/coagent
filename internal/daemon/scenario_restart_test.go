package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// A crash after the tool-bearing response commits clears confirmation state
// before execution; the restarted daemon settles the interrupted call
// per ADR-0059 and the next no-tool stop opens a fresh completion check.
func TestHarnessScenario_CompletionCheckCrashAfterToolPersistenceRestartsClean(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "completion-tool-crash.db")

	first := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, tool.IDSleep) {
				return textReply("after sleep")
			}

			return callReply("sleep-call", tool.IDSleep, `{"duration":"10m","reason":"hold the turn"}`)
		}},
	)

	first.startInboxWake()
	root, err := first.mgr.Send(first.ctx, first.projectID, "sleep then answer", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	firstCollector := collectEvents(t, first.mgr.bus.SubscribeAll())
	defer firstCollector.stop()
	firstCollector.waitWait(root, sessionevent.WaitSleep)

	// The tool-bearing response committed with its cleared completion state;
	// the daemon dies before the sleep resolves.
	state, stateErr := first.store.LoadCompletionCheckState(first.ctx, root)
	require.NoError(t, stateErr)
	assert.Nil(t, state.CandidateID, "a tool-bearing response clears any pending check")
	first.shutdown()

	second := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasUserContaining(messages, "<interrupted>") {
				return textReply("settled interrupted call")
			}

			return textReply("fresh confirmation after crash")
		}},
	)
	defer second.shutdown()
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)

	restarted, err := second.store.LoadCompletionCheckState(second.ctx, root)
	require.NoError(t, err)
	assert.Nil(t, restarted.CandidateID, "no pending check survives beside a durable tool request")
}

func TestHarnessScenario_RestartResumesExplicitInputQueuedOnStoppedRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-explicit-resume.db")
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "retained process fact"))
		require.True(t, hasUserContaining(messages, "explicit resume after stop"))

		return textReply("stopped root resumed after restart")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	_, err = first.store.Enqueue(
		first.ctx,
		sessionstore.Input{
			SessionID:  root.ID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "retained process fact",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	_, err = first.store.Enqueue(
		first.ctx,
		sessionstore.Input{
			SessionID: root.ID,
			Source:    sessionstore.InputSourceUser,
			Content:   "explicit resume after stop",
		},
	)
	require.NoError(t, err)
	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	collector.waitMessage(root.ID, "stopped root resumed after restart")
	drainScenarioClaims(t, "explicit_stopped_resume_restart.json", newChainController(t, second))
	collector.waitIdleAfter(root.ID, "stopped root resumed after restart")
	// The explicit resume turn still runs the two-phase check: candidate
	// response, then the confirmation call after the host nudge.
	assert.Equal(t, int64(2), modelCalls.Load())
	assertHarnessTrace(t, "explicit_stopped_resume_restart.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_RestartConsumesReadOnlyInputQueuedOnStoppedRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-read-only.db")
	var modelCalls atomic.Int64
	respond := func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)

		return textReply("must not call model")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	_, err = first.store.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/help"},
	)
	require.NoError(t, err)
	asyncInput, err := first.store.Enqueue(
		first.ctx,
		sessionstore.Input{
			SessionID:  root.ID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "retained process fact",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	beforeRecovery := second.session(root.ID)
	preserveStopped, err := second.mgr.commandOnlyStoppedRoot(second.ctx, beforeRecovery)
	require.NoError(t, err)
	require.True(t, preserveStopped)
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	collector.waitFor(t, "session help", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == root.ID && event.Notification.Type == sessionevent.NotifyMessage &&
				strings.Contains(event.Notification.Message, "## Session commands")
		})
	})
	drainScenarioClaims(t, "stopped_read_only_restart.json", newChainController(t, second))
	second.waitUntil("read-only recovery preserved stop", func() bool {
		record, getErr := second.store.GetSession(second.ctx, root.ID)

		return getErr == nil && record.Status == sessionstore.SessionStatusStopped &&
			!second.mgr.HasActiveLoop(root.ID)
	})

	record := second.session(root.ID)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	assert.Zero(t, modelCalls.Load())
	pending, err := second.store.PeekPending(second.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, asyncInput.Input.ID, pending.ID)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
	assertHarnessTrace(t, "stopped_read_only_restart.json", collector.snapshot(), root.ID)
}

func TestScenario_RestartResumesExplicitInputQueuedOnErroredChild(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "errored-child-explicit-resume.db")
	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("must remain queued")
	}})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := first.mgr.links.Create(first.ctx, subagent.Create{
		ProjectID: first.projectID, ParentID: root.ID, RootID: root.ID,
		Model: "fake-model", TaskCallID: "errored-child", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	require.NoError(
		t,
		seedTerminalChild(
			first.ctx,
			first.store,
			childID,
			subagent.StateError,
			"old failure",
			subagent.OutcomeError,
		),
	)
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, childID, sessionstore.SessionStatusError,
	))
	link := first.link(childID)
	require.NotNil(t, link)
	won, err := first.mgr.links.DeliverBackgroundCompletion(first.ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)
	_, err = first.store.Enqueue(
		first.ctx,
		sessionstore.Input{
			SessionID: childID,
			Source:    sessionstore.InputSourceAgent,
			Content:   "explicit retry after error",
		},
	)
	require.NoError(t, err)
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("must remain queued")
	}})
	defer second.shutdown()
	for i := range maxChildren {
		require.True(t, second.mgr.runners.tryAdmit(true, int64(30_000+i)))
		defer second.mgr.runners.release(true, int64(30_000+i))
	}

	resumed, err := second.mgr.resumeSessionsWithRecoverableInput(second.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, resumed)
	link = second.link(childID)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateRunning, link.State)
	assert.Equal(t, int64(2), link.ActivationSeq)
	pending, err := second.store.PeekPending(second.ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "explicit retry after error", pending.RawContent)
}

func TestHarnessScenario_RestartResumesAcceptedInput(t *testing.T) {
	cases := []struct {
		name         string
		toolProgress bool
		traceName    string
		sourceTest   string
	}{
		{
			name: "without assistant", traceName: "accepted_input_restart_recovery.json",
			sourceTest: "TestHarnessScenario_RestartResumesAcceptedInputWithoutAssistant",
		},
		{
			name: "after tool result", toolProgress: true,
			traceName:  "accepted_input_tool_progress_restart.json",
			sourceTest: "TestHarnessScenario_RestartResumesAcceptedInputAfterToolResult",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var afterInput func(*testing.T, *harness, int64)
			if tt.toolProgress {
				afterInput = appendCrashToolProgress
			}
			runAcceptedInputRestartScenario(t, afterInput, tt.traceName, tt.sourceTest)
		})
	}
}

func TestHarnessScenario_RestartSettlesPersistedFinalWithoutRepublishing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persisted-final.db")
	var modelCalls atomic.Int64
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return textReply("must not run")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := first.store.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "answered before crash"},
	)
	require.NoError(t, err)
	_, err = first.store.Commit(
		first.ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "[user] answered before crash",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)
	final, err := first.store.Commit(
		first.ctx,
		sessionstore.Commit{
			SessionID: root.ID,
			Messages:  []*transcript.Message{{Role: llmwire.RoleAssistant, Content: "persisted final"}},
		},
	)
	require.NoError(t, err)
	_, err = first.store.Commit(
		first.ctx,
		sessionstore.Commit{
			SessionID: root.ID,
			State:     sessionstore.StatePatch{ConfirmedAnswerID: &final.MessageIDs[0]},
		},
	)
	require.NoError(t, err)

	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	collector.waitFor(t, "persisted final settled", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == root.ID &&
				event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == sessionevent.StateIdle
		})
	})

	assert.Zero(t, modelCalls.Load(), "a durable final answer must not call the model again")
	assert.Zero(t, countPublishedMessage(collector.snapshot(), root.ID, "persisted final"),
		"historical output is state, not a new publication")
	record := second.session(root.ID)
	assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
	assertHarnessTrace(t, "accepted_input_persisted_final_restart.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_RestartDoesNotRunHandledHeaderOnlySession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "handled-header.db")
	var modelCalls atomic.Int64
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return textReply("must not run")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{SessionID: root.ID, Messages: []*transcript.Message{{
		Role: llmwire.RoleUser, Content: "User preferences from AGENTS.md files:\n\nheader only",
	}}})
	require.NoError(t, err)
	input, err := first.store.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/status"},
	)
	require.NoError(t, err)
	require.NoError(t, func() error {
		_, err := first.store.Commit(
			first.ctx,
			sessionstore.Commit{
				SessionID: input.Input.SessionID,
				Accept: []sessionstore.Accept{
					{
						InputID: input.Input.ID,
						State:   sessionstore.InputStateHandled,
						Reason:  "status command",
						LinkRef: -1,
					},
				},
			},
		)
		return err
	}())
	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	assert.False(t, second.mgr.HasActiveLoop(root.ID))
	assert.Zero(t, modelCalls.Load())
	assert.Empty(t, collector.snapshot(), "handled control input must not create recovery events")
}

func TestIntegration_SweepRedeliversIdempotently(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	defer h.shutdown()

	ctx := h.ctx

	// Seed a parent + a child that finished but whose completion was never
	// delivered (delivered_at IS NULL) — i.e. the daemon died mid-delivery.
	parent, err := h.store.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID := h.createChild(parent.ID, subagent.Link{
		TaskCallID: "orphan-call",
	})
	// Child wrote its final message before dying; its result was stored on the link
	// at terminalization (as a real run would), so delivery reads the column.
	_, err = h.store.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, Content: "child finished: 99",
	}}})
	require.NoError(t, err)
	require.NoError(
		t,
		seedTerminalChild(
			ctx,
			h.store,
			childID,
			subagent.StateCompleted,
			"child finished: 99",
			subagent.OutcomeCompleted,
		),
	)
	require.NoError(t, h.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	// First sweep delivers exactly one completion.
	h.startInboxWake()
	h.mgr.resumeAfterRestart(ctx)
	h.waitUntil(
		"child delivery",
		func() bool { delivery := h.link(childID); return delivery != nil && delivery.DeliveredAt != 0 },
	)
	h.waitUntil("parent completions", func() bool {
		count := 0
		needle := "child_id: " + strconv.FormatInt(childID, 10)
		for _, message := range h.messages(parent.ID) {
			if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
				strings.Contains(message.Content, needle) {
				count++
			}
		}
		return count >= 1
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parent.ID) })

	msgs := h.messages(parent.ID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, func(messages []llmwire.Message, childID int64) int {
		needle := "child_id: " + strconv.FormatInt(childID, 10)
		count := 0
		for _, message := range messages {
			if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
				strings.Contains(message.Content, needle) {
				count++
			}
		}

		return count
	}(msgs, childID), "exactly one record after first sweep")

	// Second sweep is idempotent: the link is now delivered (delivered_at set by the
	// atomic CAS), so it is excluded from the undelivered set and re-injects nothing
	// — still exactly one record, never zero.
	h.startInboxWake()
	h.mgr.resumeAfterRestart(ctx)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parent.ID) })

	msgs = h.messages(parent.ID)
	assert.Equal(
		t,
		1,
		func(messages []llmwire.Message, childID int64) int {
			needle := "child_id: " + strconv.FormatInt(childID, 10)
			count := 0
			for _, message := range messages {
				if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
					strings.Contains(message.Content, needle) {
					count++
				}
			}

			return count
		}(msgs, childID),
		"still exactly one record after second sweep (never zero)",
	)

	// The delivered completion reflects the stored result + outcome.
	require.Contains(t, func(messages []llmwire.Message, childID int64) string {
		needle := "child_id: " + strconv.FormatInt(childID, 10)
		for _, message := range slices.Backward(messages) {
			if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
				strings.Contains(message.Content, needle) {
				return message.Content
			}
		}

		return ""
	}(msgs, childID), "outcome: completed")
}

// Restart reconciliation resumes the same live root and patches a recreated
// service topic without inserting a duplicate.
func TestManagementRoot_RestartResumesSameRootAndPatchesTopic(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "mgmt.db")

	h := newHarness(t, harnessOptions{dbPath: dbPath, respond: trivialRespond})
	sessions := h.store
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc := h.mgr
	factory := newTestController(svc, cfg, nil, nil)

	first, err := factory.ForManager("tg-main").EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
		TopicID: 7001,
	})
	require.NoError(t, err)

	second, err := factory.ForManager("tg-main").EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
		TopicID: 7002,
	})
	require.NoError(t, err)
	assert.Equal(t, first, second, "restart with a recreated topic resumes the same root")

	record, err := sessions.GetSession(ctx, second)
	require.NoError(t, err)
	raw, ok := record.Attributes[controllerapi.SessionAttributeTelegramTopicID]
	require.True(t, ok, "the topic binding is patched to the current service topic")
	assert.Equal(t, int64(7002), topicAttrNumber(raw))
}

// The invariant, stated as a comparison between two independent views: the
// ledger-keyed and name-keyed pending sets must agree; every pending external
// call has an owner able to resolve it, across restart.
func TestHarnessModel_PendingExternalCallOwnershipAgreesAfterRestart(t *testing.T) {
	assertAgrees := func(t *testing.T, d *applyDaemon, sessionID int64) {
		t.Helper()

		owners, err := d.mgr.callOwners(d.ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, unresolvedExternalCallsByName(d.messages(sessionID)), owners,
			"an unresolved external call the provider can see must have a producer that can resolve it")
	}

	t.Run("a task loses its producer and is closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "orphan.db")
		configDir := newApplyConfigDir(t)

		var seen modelRequests

		sessionID := stageTaskAndStop(t, dbPath, configDir, &seen)

		second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
		defer second.shutdown()

		require.NoError(t, second.mgr.Start(second.ctx))
		second.waitUntil("orphaned task result consumed", func() bool {
			return countToolResultsFor(second.messages(sessionID), tool.IDTask) == 1
		})

		assertAgrees(t, second, sessionID)

		msgs := second.messages(sessionID)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDTask),
			"the orphaned task is closed exactly once")
	})

	t.Run("a task keeps its live child", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "task.db")
		configDir := newApplyConfigDir(t)

		var seen modelRequests

		first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

		first.startInboxWake()
		sessionID, err := first.mgr.Send(first.ctx, first.projectID, "do work then spawn", "fake-model", nil)
		require.NoError(t, err)

		first.waitUntil("the parent parked on the child", func() bool {
			return countAssistantToolCallsFor(first.messages(sessionID), tool.IDTask) == 1 &&
				!first.mgr.HasActiveLoop(sessionID)
		})

		// No shutdown: the child link is still live, so the sweep must leave
		// the call pending instead of closing it as orphaned.
		first.mgr.resumeAfterRestart(first.ctx)

		assertAgrees(t, first, sessionID)
		assert.Zero(t, countToolResultsFor(first.messages(sessionID), tool.IDTask),
			"a call whose child survived must stay pending")

		first.shutdown()
	})

	t.Run("a config apply keeps its marker", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "apply.db")
		configDir := newApplyConfigDir(t)

		sessionID := stageApplyAndStop(t, dbPath, configDir)

		second := newApplyDaemon(t, dbPath, configDir)
		defer second.shutdown()

		require.NoError(t, second.mgr.Start(second.ctx))

		assertAgrees(t, second, sessionID)
		assert.Zero(t, countToolResultsFor(second.messages(sessionID), tool.IDConfigEdit),
			"the marker still owes this call its verdict")

		_, err := second.bootVerdict(t)
		require.NoError(t, err, "the verdict must still reach the call the sweep left alone")
	})

	t.Run("a config apply whose marker did not survive", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "apply-nomarker.db")
		configDir := newApplyConfigDir(t)

		sessionID := stageApplyAndStop(t, dbPath, configDir)
		require.NoError(t, os.Remove(filepath.Join(configDir, coagenthome.PendingApplyFileName)))

		second := newApplyDaemon(t, dbPath, configDir)
		defer second.shutdown()

		require.NoError(t, second.mgr.Start(second.ctx))
		second.waitUntil("orphaned config result consumed", func() bool {
			return countToolResultsFor(second.messages(sessionID), tool.IDConfigEdit) == 1
		})

		assertAgrees(t, second, sessionID)

		msgs := second.messages(sessionID)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit),
			"a verdict nobody can produce must be closed, not left dangling")

		// The apply slot is in-memory, so a claim the previous image never gave
		// back cannot reach this one: only a strand inside one image is dangerous.
		assert.True(t, second.mgr.applier.ClaimApply(),
			"a new image starts with a free apply slot")
	})
}

// The whole observable story of a blocking child lost to a restart: the parent
// parks on the task, the daemon restarts, and the next thing the user types must
// reach the model on a transcript a provider accepts — not a dangling tool_use.
func TestScenario_BlockingTaskOrphanedByARestartIsClosedOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "task.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

	first.startInboxWake()
	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return first.parkedOnChild(sessionID)
	})

	// The child never finishes; the restart must not resurrect it as a producer.
	link := first.linkByCall(sessionID, orphanTaskCallID)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")
	_, err = first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, err)

	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("orphan cancellation consumed", func() bool {
		return countToolResultsFor(second.messages(sessionID), tool.IDTask) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})

	recovered := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(recovered), "boot must leave a transcript a provider accepts")
	require.Equal(t, 1, countToolResultsFor(recovered, tool.IDTask),
		"the abandoned task is closed exactly once")
	assert.Contains(t, func(msgs []llmwire.Message, toolName string) string {
		for _, v := range slices.Backward(msgs) {
			if v.Role == llmwire.RoleTool && v.ToolName == toolName {
				return v.Content
			}
		}

		return ""
	}(recovered, tool.IDTask), "restarted")

	second.startInboxWake()
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "any progress?"))
	second.waitUntil("follow-up consumed", func() bool {
		return hasUserContaining(second.messages(sessionID), "any progress?") &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })

	final := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask))
	assert.True(t, hasUserContaining(final, "any progress?"), "the user's message reaches the conversation")
	assert.Contains(t, lastAssistantTextDTO(final), "restarted", "the model reacts to the cancelled task")

	seen.assertAllPaired(t)
}

// Managers and the schedule executor start once Start returns, and a runner
// either of them opens makes PASS 0 skip that session for the rest of the boot.
// So PASS 0 has to be finished by then, not merely spawned.
func TestScenario_OrphanedCallsAreClosedBeforeStartReturns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boot.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	sessionID := stageTaskAndStop(t, dbPath, configDir, &seen)

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	if countToolResultsFor(second.messages(sessionID), tool.IDTask) == 0 {
		pending, err := second.store.ListPending(second.ctx, sessionID)
		require.NoError(t, err)
		queued := slices.ContainsFunc(pending, func(input *sessionstore.InboxInput) bool {
			return input.Source == sessionstore.InputSourceCallResult &&
				input.Attributes["call_id"] == orphanTaskCallID && input.Attributes["tool_id"] == tool.IDTask
		})
		require.True(t, queued || countToolResultsFor(second.messages(sessionID), tool.IDTask) == 1,
			"boot commits the exact cancellation before controllers can open a runner")
	}
	second.waitUntil("boot orphan result consumed", func() bool {
		return countToolResultsFor(second.messages(sessionID), tool.IDTask) == 1
	})

	msgs := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs),
		"a controller may open a runner the instant Start returns")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDTask),
		"the orphaned task is closed by the time Start returns")
}

// A message that waited behind the task is the user's turn, not the
// daemon's: the restart that lost the child must not swallow it either.
func TestScenario_MessageQueuedBehindAnOrphanedTaskRunsAfterRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queued.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests
	// The child must stay parked, or its completion wakes the parent before
	// the queued message is ever observed waiting behind the task.
	childHeld := make(chan struct{})
	heldRespond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		if !hasToolResultFor(msgs, tool.IDTask) && hasUserContaining(msgs, "do the thing") {
			<-childHeld
		}

		return askForBlockingTaskRespond(system, msgs)
	}

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(heldRespond))
	defer func() {
		select {
		case <-childHeld:
		default:
			close(childHeld)
		}
	}()

	first.startInboxWake()
	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return first.parkedOnChild(sessionID)
	})

	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionID, "any progress?"))
	first.waitUntil("the message waits behind the task", func() bool {
		_, pendErr := first.store.PeekPending(first.ctx, sessionID)

		return pendErr == nil && !first.mgr.HasActiveLoop(sessionID)
	})
	require.False(t, hasUserContaining(first.messages(sessionID), "any progress?"),
		"a user turn may not split a tool_use from its result")

	link := first.linkByCall(sessionID, orphanTaskCallID)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")
	_, delErr := first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, delErr)

	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("the queued message finally ran", func() bool {
		return hasUserContaining(second.messages(sessionID), "any progress?")
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })

	final := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask))
	second.requireInboxDrained(sessionID)

	seen.assertAllPaired(t)
}

func TestHarnessScenario_ProcessCrashRestartDeliversInterruptedOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process-crash-restart.db")
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))
		require.True(t, hasUserContaining(messages, "state: interrupted"))

		return textReply("interrupted process recovered")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusCompleted,
	))
	outputPath := filepath.Join(t.TempDir(), "interrupted.output")
	require.NoError(t, os.WriteFile(outputPath, []byte("partial output\n"), 0o600))
	now := time.Now().UTC()
	process := backgroundprocess.Process{
		ID: "crashed-process", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "crashed-call", OutputPath: outputPath, CreatedAt: now,
		Deadline: now.Add(time.Minute), AdvertisedAt: &now, OutputSize: 15,
		State: backgroundprocess.StateRunning,
	}
	require.NoError(t, first.mgr.processStore.InsertProcess(first.ctx, process))

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
		first.shutdown()
	}()
	require.NoError(t, second.mgr.Start(second.ctx))
	collector.waitMessage(root.ID, "interrupted process recovered")
	drainScenarioClaims(t, "process_crash_restart.json", newChainController(t, second))
	collector.waitIdleAfter(root.ID, "interrupted process recovered")

	final, err := second.mgr.processStore.GetProcess(second.ctx, process.ID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateInterrupted, final.State)
	// The recovered wake opens the two-phase check; the confirmation call
	// publishes the recovery answer.
	assert.Equal(t, int64(2), modelCalls.Load())
	var inputs int
	require.NoError(t, second.db.QueryRowContext(second.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, process.ID).Scan(&inputs))
	assert.Equal(t, 1, inputs)
	assertHarnessTrace(t, "process_crash_restart.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_ForegroundBashCrashRestartResolvesInterruptedCall(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "foreground-bash-crash-restart.db")
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		if hasToolResultFor(messages, "bash") {
			return textReply("interrupted bash resolved")
		}

		return textReply("unexpected activation")
	}

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root, err := first.store.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := first.store.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "run the command"},
	)
	require.NoError(t, err)
	_, err = first.store.Commit(
		first.ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "run the command",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)

	toolCalls, err := json.Marshal([]llmwire.ToolCall{
		{ID: "fg-bash", Name: "bash", Arguments: []byte(`{"command":"printf 'hello\\n'"}`)},
		{ID: "fg-read", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
	})
	require.NoError(t, err)
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{SessionID: root.ID, Messages: []*transcript.Message{{
		Role: "assistant", Content: "running the command", ToolCalls: toolCalls,
	}}})
	require.NoError(t, err)

	now := time.Now().UTC()
	process := backgroundprocess.Process{
		ID: "crashed-fg-bash", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "fg-bash", OutputPath: filepath.Join(t.TempDir(), "fg-bash.output"),
		CreatedAt: now, Deadline: now.Add(time.Minute), OutputSize: 0,
		State: backgroundprocess.StateRunning,
	}
	require.NoError(t, first.mgr.processStore.InsertProcess(first.ctx, process))

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	collector := collectEvents(t, second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
		first.shutdown()
	}()
	require.NoError(t, second.mgr.Start(second.ctx))
	collector.waitMessage(root.ID, "interrupted bash resolved")
	collector.waitIdleAfter(root.ID, "interrupted bash resolved")

	final, err := second.mgr.processStore.GetProcess(second.ctx, process.ID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateInterrupted, final.State)
	// The recovered wake's stop opens the two-phase check; the confirmation
	// call publishes the resolved answer.
	assert.Equal(t, int64(2), modelCalls.Load())

	var bashResults, readResults int
	for _, message := range second.messages(root.ID) {
		if message.Role != llmwire.RoleTool {
			continue
		}

		switch message.ToolName {
		case "bash":
			bashResults++
			assert.True(t, message.ToolError, "interrupted bash must resolve as a typed failure")
		case "read":
			readResults++
			assert.True(t, message.ToolError, "interrupted read must resolve as a typed failure")
		}
	}
	assert.Equal(t, 1, bashResults)
	assert.Equal(t, 1, readResults)
}

func TestScenario_InterruptedCallSettlementNeedsNoProjectOrModel(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("unused")
	}})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "removed-model", "", nil)
	require.NoError(t, err)
	toolCalls, err := json.Marshal([]llmwire.ToolCall{{
		ID: "interrupted-bash", Name: "bash", Arguments: []byte(`{"command":"true"}`),
	}})
	require.NoError(t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{SessionID: root.ID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: toolCalls,
	}}})
	require.NoError(t, err)

	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	require.NoError(t, os.Rename(workDir, workDir+".gone"))

	err = h.mgr.settleUnresolvedCalls(h.ctx)
	require.NoError(t, err)

	messages, err := h.store.LoadActiveMessages(h.ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, llmwire.RoleTool, messages[1].Role)
	assert.Equal(t, "interrupted-bash", messages[1].ToolCallID)
	assert.True(t, messages[1].ToolError)
}

func TestResponseIntegrity_RestartAfterFirstLengthPerformsOnlyOwedRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "response-restart.db")
	rootID := interruptFirstRecovery(t, dbPath)
	completeRecoveryAfterRestart(t, dbPath, rootID)
}

func TestResponseIntegrity_RestartAfterTerminalFailureDoesNotRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "response-terminal-restart.db")
	first := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			return &llmwire.Response{Text: "discarded terminal", FinishType: llmwire.FinishUnknown}
		}},
	)
	first.startInboxWake()
	rootID, err := first.mgr.Send(first.ctx, first.projectID, "fail terminally", "fake-model", nil)
	require.NoError(t, err)
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(rootID) })
	record := first.session(rootID)
	require.Equal(t, sessionstore.SessionStatusError, record.Status)
	first.shutdown()

	var unexpectedCalls atomic.Int64
	second := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			unexpectedCalls.Add(1)
			return &llmwire.Response{Text: "unexpected rerun", FinishType: llmwire.FinishStop}
		}},
	)
	defer second.shutdown()
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	assert.Never(t, func() bool { return unexpectedCalls.Load() != 0 }, 300*time.Millisecond, 10*time.Millisecond)
}

// This is the production incident shape: an ordinary in-loop bash call is
// cancelled, not an externally owned call. Stop must settle the transcript so a
// later input starts fresh instead of reconstructing and re-running bash.
func TestScenario_StopOrdinaryBashDoesNotReplayAfterNewInputOrRestart(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "what happened?") {
			return textReply("fresh response")
		}
		if countAssistantToolCallsFor(msgs, "bash") > 0 {
			return textReply("old work was interrupted")
		}
		return callReply("long-bash", "bash", `{"command":"sleep 30"}`)
	}

	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same daemon", true: "after restart"}[restart], func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "ordinary-bash.db")
			first := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
			first.startInboxWake()
			firstID, err := first.mgr.Send(first.ctx, first.projectID, "watch checks", "fake-model", nil)
			require.NoError(t, err)
			first.waitUntil("ordinary bash started", func() bool {
				return countAssistantToolCallsFor(first.messages(firstID), "bash") == 1 &&
					first.mgr.HasActiveLoop(firstID)
			})

			require.NoError(t, first.mgr.sendToSession(first.ctx, firstID, "/stop"))
			first.waitUntil("stop completed", func() bool {
				rec, getErr := first.store.GetSession(first.ctx, firstID)
				return getErr == nil && rec.Status == sessionstore.SessionStatusStopped
			})

			active := first.messages(firstID)
			require.NoError(t, llm.ValidateToolPairing(active))
			assert.Equal(t, 1, countToolResultsFor(active, "bash"))

			d := first
			if restart {
				first.shutdown()
				d = newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
				require.NoError(t, d.mgr.Start(d.ctx))
				defer d.shutdown()
			}

			d.startInboxWake()
			require.NoError(t, d.mgr.sendToSession(d.ctx, firstID, "what happened?"))
			d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(firstID) })
			final := d.messages(firstID)
			assert.Equal(t, 1, countAssistantToolCallsFor(final, "bash"))
			assert.Equal(t, 1, countToolResultsFor(final, "bash"))
			assert.True(t, hasUserContaining(final, "what happened?"))
			assert.Contains(t, func(messages []llmwire.Message) []string {
				var texts []string
				for _, message := range messages {
					if message.Role == llmwire.RoleAssistant && message.Content != "" {
						texts = append(texts, message.Content)
					}
				}
				return texts
			}(final), "fresh response")
		})
	}
}

// /stop is a durable two-phase park, so its second phase may run in a process
// that never saw the call staged. The stopped session is still resumable, so its
// transcript must be provider-valid when it comes back.
func TestScenario_StopThatDidNotSurviveItsRestartStillSettlesTheOrphan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-task.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

	first.startInboxWake()
	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return countAssistantToolCallsFor(first.messages(sessionID), tool.IDTask) == 1 &&
			!first.mgr.HasActiveLoop(sessionID)
	})

	// Phase one of /stop lands; the process dies before phase two settles anything.
	require.NoError(t, first.store.UpdateSessionStatus(
		first.ctx, sessionID, sessionstore.SessionStatusStopping,
	))
	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)

	rec := second.session(sessionID)
	require.Equal(t, sessionstore.SessionStatusStopped, rec.Status, "the recovery finished the park")

	recovered := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(recovered),
		"a resumable stopped session must carry a transcript a provider accepts")
	require.Equal(t, 1, countToolResultsFor(recovered, tool.IDTask),
		"the abandoned task is settled exactly once")

	second.startInboxWake()
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "any progress?"))
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })

	final := second.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask),
		"the suspended call is answered, never re-executed")
	assert.True(t, hasUserContaining(final, "any progress?"))

	seen.assertAllPaired(t)
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
			name:       "background child arrives through the parent inbox",
			background: true,
			completions: func(messages []llmwire.Message, childID int64) int {
				needle := "child_id: " + strconv.FormatInt(childID, 10)
				count := 0
				for _, message := range messages {
					if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
						strings.Contains(message.Content, needle) {
						count++
					}
				}

				return count
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "crash.db")

			first := newHarness(t, harnessOptions{dbPath: dbPath, respond: crashWindowRespond(tc.background)})
			gate := newCrashGate(first.mgr.links)
			first.mgr.links = gate

			first.startInboxWake()
			parentID, err := first.mgr.Send(first.ctx, first.projectID, "spawn a child", "fake-model", nil)
			require.NoError(t, err)

			first.waitUntil("child link", func() bool { return first.linkByCall(parentID, taskCallID) != nil })
			link := *first.linkByCall(parentID, taskCallID)

			select {
			case <-gate.rejected:
			case <-time.After(5 * time.Second):
				t.Fatal("daemon A never reached the owed-completion window")
			}

			first.shutdown()

			second := newHarness(t, harnessOptions{dbPath: dbPath, respond: crashWindowRespond(tc.background)})
			defer second.shutdown()

			owed := second.link(link.ChildID)
			require.NotNil(t, owed)
			require.True(t, owed.Terminal(), "the child was finalized before the crash")
			require.Zero(t, owed.DeliveredAt, "the completion handoff never committed before the crash")
			require.Zero(t, tc.completions(second.messages(parentID), link.ChildID),
				"the completion must not reach the transcript before the restart")

			require.NoError(t, second.mgr.Start(second.ctx))

			// Waiting for one completion is not enough: it becomes visible when
			// the parent's turn accepts it, before the turn's own reply commits.
			second.waitUntil("sweep redelivered the owed completion", func() bool {
				return lastAssistantTextDTO(second.messages(parentID)) == "parent got the child result"
			})
			second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(parentID) })

			msgs := second.messages(parentID)
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
			return textReply("child must never finish")
		}

		if hasToolResultFor(msgs, "task") {
			return textReply("child launched")
		}

		return callReply(
			taskCallID,
			"task",
			`{"prompt":"CHILD_TASK hang","description":"c","subagent_type":"general","background":true}`,
		)
	}

	// Daemon B's child would answer immediately, so a wrongly resumed child shows
	// up as a real delivery rather than as another hang.
	eager := crashWindowRespond(true)

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: held})

	first.startInboxWake()
	parentID, err := first.mgr.Send(first.ctx, first.projectID, "spawn a child", "fake-model", nil)
	require.NoError(t, err)

	first.waitUntil("child link", func() bool { return first.linkByCall(parentID, taskCallID) != nil })
	link := *first.linkByCall(parentID, taskCallID)
	first.waitUntil("child loop is live", func() bool { return first.mgr.HasActiveLoop(link.ChildID) })

	require.NoError(t, first.mgr.sendToSession(first.ctx, link.ChildID, "/stop"))

	parked := first.link(link.ChildID)
	require.Equal(t, subagent.StateStopped, parked.State)

	first.shutdown()

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: eager})
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)

	require.Never(t, func() bool {
		current, linkErr := second.links.GetLink(second.ctx, link.ChildID)
		return linkErr == nil && current != nil &&
			(current.State != subagent.StateStopped || current.DeliveredAt != 0)
	}, 500*time.Millisecond, 25*time.Millisecond, "a stopped child stays parked across a restart")

	assert.Zero(t, func(messages []llmwire.Message, childID int64) int {
		needle := "child_id: " + strconv.FormatInt(childID, 10)
		count := 0
		for _, message := range messages {
			if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
				strings.Contains(message.Content, needle) {
				count++
			}
		}

		return count
	}(second.messages(parentID), link.ChildID),
		"a stopped child owes the parent nothing")
	assert.False(t, second.mgr.HasActiveLoop(link.ChildID), "the sweep must not start a stopped child")
}

// TestScenario_OwnedTaskCallSurvivesSchemaUpgradeAndRestarts stages a version-30
// database: a runnable foreground child whose link owns the parent's task call,
// the link still carrying a legacy nonzero timeout_sec. After the boot migration
// drops the column, the task tool must not re-execute, the link must still own
// the call, and the pre-existing child must complete exactly once.
func TestScenario_OwnedTaskCallSurvivesSchemaUpgradeAndRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	ctx := context.Background()

	// Stage the pre-upgrade state at version 30.
	staged, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.RunUpTo(ctx, staged, 30))

	_, err = staged.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, ?, 'p')`, t.TempDir())
	require.NoError(t, err)

	stagedStore := sessionstore.NewStore(staged)
	parent, err := stagedStore.CreateSession(ctx, 1, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, stagedStore.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusSuspended))

	// Stage through raw v30-shaped inserts: the v30 schema predates the
	// tool_error column, so the session store's insert (which writes it)
	// cannot run against this staged database.
	_, err = staged.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content) VALUES (?, 'user', 'kick off the work')`,
		parent.ID)
	require.NoError(t, err)

	// Arguments serialize exactly as the session store persists them, with the
	// legacy "timeout" key the current schema no longer decodes into anything.
	stagedArgs, err := json.Marshal(llmwire.ToolCall{
		ID: "tc_staged", Name: tool.IDTask, Arguments: []byte(`{"prompt":"CHILD_STAGED","timeout":1}`),
	})
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content, tool_calls) VALUES (?, 'assistant', '', ?)`,
		parent.ID, "["+string(stagedArgs)+"]")
	require.NoError(t, err)

	result, err := staged.ExecContext(ctx, `INSERT INTO sessions
		(project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at)
		VALUES (1, ?, ?, 'general', 'fake-model', 'medium', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		parent.ID, parent.ID)
	require.NoError(t, err)
	childID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at, timeout_sec)
		VALUES (?, ?, 'tc_staged', 1, 1, 'spawned', 1, 1)`, parent.ID, childID)
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, received_at)
		VALUES (?, 'agent', 'CHILD_STAGED do the staged work', CURRENT_TIMESTAMP)`, childID)
	require.NoError(t, err)

	var linkCount int
	require.NoError(t, staged.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subagent_links`).Scan(&linkCount))
	require.Equal(t, 1, linkCount)
	require.NoError(t, staged.Close())

	// Boot a real daemon on the staged database: Run applies every later migration.
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_STAGED") {
			return textReply("staged child finished")
		}

		return textReply("parent done")
	}
	h := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	defer h.shutdown()

	require.NoError(t, h.mgr.Start(h.ctx))
	h.startInboxWake()
	h.mgr.resumeAfterRestart(h.ctx)

	var timeoutCol int
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('subagent_links') WHERE name = 'timeout_sec'`).Scan(&timeoutCol))
	assert.Zero(t, timeoutCol, "the boot migration must drop the legacy column")

	link := h.linkByCall(parent.ID, "tc_staged")
	require.NotNil(t, link)
	assert.Equal(t, childID, link.ChildID,
		"the staged link still owns the call — no new child may spawn")

	// The child completes and delivers exactly once to the suspended parent.
	h.waitUntil(
		"child delivery",
		func() bool { delivery := h.link(childID); return delivery != nil && delivery.DeliveredAt != 0 },
	)
	h.waitUntil("the parent consumed the staged child's completion", func() bool {
		return countToolResultsFor(h.messages(parent.ID), tool.IDTask) == 1 &&
			!h.mgr.HasActiveLoop(parent.ID)
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parent.ID) })

	final := h.messages(parent.ID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask),
		"the task call is never re-issued")
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask),
		"exactly one completion fills the staged call")
	require.Contains(t, func(msgs []llmwire.Message, toolName string) string {
		for _, v := range slices.Backward(msgs) {
			if v.Role == llmwire.RoleTool && v.ToolName == toolName {
				return v.Content
			}
		}

		return ""
	}(final, tool.IDTask), "staged child finished")

	var spawnCount int
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`, parent.ID).Scan(&spawnCount))
	assert.Equal(t, 1, spawnCount, "recovery spawns no replacement child")

	link = h.link(childID)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
}
