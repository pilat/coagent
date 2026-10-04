package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestManagerBoundControllerRejectsEveryForeignSessionOperation(t *testing.T) {
	t.Parallel()

	testFactory := &mockFactory{}

	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})

	mgr := testHarness.mgr

	store := testHarness.store
	ctx := context.Background()
	projectID := testProject(t, store, "/tmp/controller-owner-operations")
	alphaRecord, err := mgr.store.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)
	betaRecord, err := mgr.store.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "beta",
	})
	require.NoError(t, err)
	controller := newTestController(mgr, &config.Config{}, nil, nil).ForManager("alpha")

	sessions, err := controller.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, alphaRecord.ID, sessions[0].ID)

	operations := []struct {
		name string
		call func() error
	}{
		{name: "send", call: func() error {
			return controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
				SessionID: betaRecord.ID, Message: "foreign",
			})
		}},
		{name: "set model", call: func() error {
			return controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
				SessionID: betaRecord.ID, Model: "other",
			})
		}},
		{name: "set attributes", call: func() error {
			return controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
				SessionID: betaRecord.ID, Attributes: map[string]any{"topic": 9},
			})
		}},
		{name: "list skills", call: func() error {
			_, listErr := controller.ListSkills(ctx, controllerapi.ConfigSkillsData{SessionID: betaRecord.ID})
			return listErr
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			require.ErrorContains(t, operation.call(), "belongs to another manager")
		})
	}

	stored, err := mgr.store.GetSession(ctx, betaRecord.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.KilledAt)
	assert.Equal(t, "model", stored.Model)
	assert.NotContains(t, stored.Attributes, "topic")
}

func TestCreateSession_PersistsOnlyTheBoundManagerOwner(t *testing.T) {
	ctx := context.Background()
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	ctrl := newTestController(mgr, &config.Config{}, nil, nil).ForManager("alpha")
	workDir := t.TempDir()

	ownedID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
			"channel":                               "test",
		},
	})
	require.NoError(t, err)
	owned, err := mgr.store.GetSession(ctx, ownedID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", owned.Attributes[controllerapi.SessionAttributeManagerID])
	assert.Equal(t, "test", owned.Attributes["channel"])

	secondID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
		},
	})
	require.NoError(t, err)
	second, err := mgr.store.GetSession(ctx, secondID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", second.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_Send(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	// Use completeAfter so Kill (which no longer cancels context) lets session finish naturally
	factory.nextSess = &mockSession{completeAfter: 200 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "hello", "", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Session should be running
	assert.True(t, mgr.HasActiveLoop(id))

	// Kill it
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_SendToSession_PersistsWhileRunning(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store

	// Subscribe to all notifications via pubsub
	ch := mgr.bus.SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Wait for RunDaemon to start
	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.store.(*sessionstore.Store), id, false, 3*time.Second)

	require.Eventually(
		t,
		func() bool { factory.mu.Lock(); defer factory.mu.Unlock(); return len(factory.sessions) == 1 },
		time.Second,
		10*time.Millisecond,
	)
	factory.mu.Lock()
	require.Len(t, factory.sessions, 1, "factory should have created one session")
	mockSess := factory.sessions[0]
	factory.mu.Unlock()

	mockSess.mu.Lock()
	ran := mockSess.ran
	mockSess.mu.Unlock()
	require.True(t, ran, "RunDaemon should have been called")

	require.NoError(t, mgr.sendToSession(ctx, id, "do something"))
	pending, err := mgr.store.PeekPending(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "do something", pending.RawContent)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendDuplicateWorkdir(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	s := testHarness.store

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Sending to the same project always creates a new session
	id2, err := mgr.Send(ctx, pid, "another", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_InputReceivedPubSub(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	s := testHarness.store

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	sub := mgr.bus.Subscribe(id)
	defer mgr.bus.Unsubscribe(id, sub)

	mgr.bus.Publish(id, sessionevent.Notification{
		Type:    sessionevent.NotifyInputReceived,
		Message: "hello from agent",
		Source:  "agent",
	})

	select {
	case n := <-sub:
		assert.Equal(t, sessionevent.NotifyInputReceived, n.Type)
		assert.Equal(t, "hello from agent", n.Message)
		assert.Equal(t, "agent", n.Source)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for input_received notification")
	}

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendCreatesNewSessionAfterCompletion(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	// First session completes quickly
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send creates a new session — no resume
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "wake up message", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "Send should create new session, not resume")

	waitForLoopStart(t, ch, id2, 3*time.Second)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendToSession_AlreadyRunningUsesDurableInbox(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.store.(*sessionstore.Store), id, false, 3*time.Second)

	// SendToSession on an already-running session — should route to inbox
	err = mgr.sendToSession(ctx, id, "steer message")
	require.NoError(t, err)

	pending, err := mgr.store.PeekPending(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "steer message", pending.RawContent)

	// Only one session should exist
	assert.True(t, mgr.HasActiveLoop(id))

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SecondSendCreatesNewLoop(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "first", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send creates a new session with its own loop
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "second", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "should create new session")

	waitForLoopStart(t, ch, id2, 3*time.Second)
	require.Eventually(t, func() bool {
		factory.mu.Lock()
		defer factory.mu.Unlock()

		return len(factory.sessions) >= 2
	}, time.Second, 10*time.Millisecond)

	factory.mu.Lock()
	sessCount := len(factory.sessions)
	factory.mu.Unlock()
	assert.GreaterOrEqual(t, sessCount, 2, "factory should have been called at least twice")

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendAlwaysCreatesNew(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	// First session completes quickly so Kill can work
	factory.nextSess = &mockSession{completeAfter: 100 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	_ = mgr.sendToSession(context.Background(), id1, "/kill")
	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send to same project must create a new session, not resume
	factory.nextSess = &mockSession{completeAfter: 100 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "Send should produce a different session ID")

	waitForLoopStart(t, ch, id2, 3*time.Second)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SetModel_IdleSession(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	// Session completes quickly → loop exits → session is idle
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "old-model", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)
	assert.False(t, mgr.HasActiveLoop(id), "session should not be running after idle")

	// SetModel on an idle session must succeed (DB-first pattern)
	err = mgr.SetModel(context.Background(), id, "new-model", "high")
	require.NoError(t, err)

	// Verify the model was persisted in SQLite
	rec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "new-model", rec.Model)
	assert.Equal(t, "high", rec.ReasoningLevel)
}

func TestManager_SendToSession_RejectsKilledSession(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Prepare a mock for the kill's resume path
	factory.mu.Lock()
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	factory.mu.Unlock()

	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	// SendToSession on a killed session must return an error
	err = mgr.sendToSession(ctx, id, "should fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "killed")
}

func TestManager_SetAttributesCannotRemoveOrRebindManagerOwner(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	store := testHarness.store
	ctx := context.Background()
	pid := testProject(t, store, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, pid, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)

	require.NoError(t, mgr.SetAttributes(ctx, rec.ID, map[string]any{"topic": float64(42)}))
	stored, err := mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
	assert.InDelta(t, float64(42), stored.Attributes["topic"], 0)

	err = mgr.SetAttributes(ctx, rec.ID, map[string]any{
		controllerapi.SessionAttributeManagerID: "beta",
	})
	require.ErrorContains(t, err, `belongs to manager "alpha"`)
	stored, err = mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_ConcurrentManagerClaimsHaveExactlyOneWinner(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	store := testHarness.store
	ctx := context.Background()
	pid := testProject(t, store, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, pid, "test-model", "", nil)
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, owner := range []string{"alpha", "beta"} {
		go func() {
			<-start
			errs <- mgr.SetAttributes(ctx, rec.ID, map[string]any{
				controllerapi.SessionAttributeManagerID: owner,
			})
		}()
	}
	close(start)

	results := []error{<-errs, <-errs}
	successes := 0
	for _, claimErr := range results {
		if claimErr == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes)

	stored, err := mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Contains(t, []string{"alpha", "beta"}, stored.Attributes[controllerapi.SessionAttributeManagerID])
}

// TestNewSessionSettlesTheEffortOnItsModel: a fresh session names no level, so it
// must start on its model's default — anything else is a level nobody chose.
func TestNewSessionSettlesTheEffortOnItsModel(t *testing.T) {
	provider := newSpawnEffortProvider(t)
	h := newHarness(
		t,
		harnessOptions{
			configure: withSpawnEffortModels(provider.url),
			clientFor: configuredClient(withSpawnEffortModels(provider.url)),
		},
	)

	defer h.shutdown()

	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "work", "parent-model", nil)
	require.NoError(t, err)
	// The two-phase check spends a hidden candidate and a confirmation, both
	// "done" from the stub, so one visible turn is two assistant rows.
	h.waitUntil("answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 2
	})
	h.mgr.waitIdle(id)

	rec, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"the record is all a later run reads, so it must carry the model's default")
	assert.Equal(t, "high", provider.effortFor("parent-model"),
		"a fresh session must ask for its model's default, not a clamped stand-in")
}

func TestSendSessionMessageResolvedFollowsOwnedReplacement(t *testing.T) {
	ctx := context.Background()
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	store := testHarness.store
	projectID, err := store.GetOrCreateProject(ctx, t.TempDir())
	require.NoError(t, err)
	old, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "cli",
	})
	require.NoError(t, err)
	newID, err := mgr.clear(ctx, lifecycleInput(ctx, t, mgr, old.ID, "/clear"))
	require.NoError(t, err)

	controller := newTestController(mgr, &config.Config{}, nil, nil).ForManager("cli")
	router, ok := controller.(controllerapi.SessionMessageRouter)
	require.True(t, ok)
	acceptedID, err := router.SendSessionMessageResolved(ctx, controllerapi.SessionMessageData{
		SessionID: old.ID, Message: "racing clear",
	})
	require.NoError(t, err)
	assert.Equal(t, newID, acceptedID)
}

// TestSetModelUnknownModelNeverReachesTheRecord drives the user-visible path: a
// model switch to an id no client can be built for must be reported to the
// caller, and must leave the session resumable.
func TestSetModelUnknownModelNeverReachesTheRecord(t *testing.T) {
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "done"}
	}

	h := newHarness(
		t,
		harnessOptions{
			configure: withKnownModels([]string{"fake-model"}),
			clientFor: knownModelClient([]string{"fake-model"}, respond),
		},
	)
	defer h.shutdown()

	events := collectEvents(h.mgr.bus.SubscribeAll())
	defer events.stop()

	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "fake-model", nil)
	require.NoError(t, err)
	// The no-wake two-phase check finishes one turn as two assistant rows: the
	// hidden candidate plus the confirmation, both "done" from the stub.
	h.waitUntil("first turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 2
	})
	h.mgr.waitIdle(id)

	err = h.mgr.SetModel(h.ctx, id, "ghost-model", "")
	require.Error(t, err, "an unknown model must be rejected, not persisted")
	assert.Contains(t, err.Error(), "ghost-model")

	rec, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", rec.Model, "the record keeps the model the session can actually run")

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn settled", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 4 || hasSessionErrorNotice(events.snapshot())
	})

	assert.False(t, hasSessionErrorNotice(events.snapshot()), "the session must still be resumable")
	assert.Equal(t, 4, countAssistantReplies(h.parentMessages(id)))
}

// TestSetModelLiveRefusalDoesNotPersist covers the running-loop branch: the live
// session is the authority on a switch, so a switch it refuses is not persisted.
func TestSetModelLiveRefusalDoesNotPersist(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HOLD") {
			<-release
		}

		return &llmwire.Response{Text: "done"}
	}

	h := newHarness(
		t,
		harnessOptions{
			configure: withKnownModels([]string{"fake-model", "other-model"}),
			clientFor: knownModelClient([]string{"fake-model", "other-model"}, respond),
		},
	)

	defer h.shutdown()
	defer close(release)

	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "HOLD please", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("live session attached", func() bool { return h.liveSession(id) != nil })

	before, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)

	h.mgr.build.Config.UnifiedConfig.Models[1].Provider = "missing"
	err = h.mgr.SetModel(h.ctx, id, "other-model", "high")
	require.Error(t, err, "a refused switch must surface to the caller")

	rec, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", rec.Model)
	assert.Equal(t, before.ReasoningLevel, rec.ReasoningLevel)
}
