package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// TestSetModelUnknownModelNeverReachesTheRecord drives the user-visible path: a
// model switch to an id no client can be built for must be reported to the
// caller, and must leave the session resumable.
func TestSetModelUnknownModelNeverReachesTheRecord(t *testing.T) {
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "done"}
	}

	h := newModelAwareHarness(t, []string{"fake-model"}, respond)
	defer h.shutdown()

	events := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer events.stop()

	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("first turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 1
	})
	h.mgr.waitIdle(id)

	err = h.mgr.SetModel(h.ctx, id, "ghost-model", "")
	require.Error(t, err, "an unknown model must be rejected, not persisted")
	assert.Contains(t, err.Error(), "ghost-model")

	rec, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", rec.Model, "the record keeps the model the session can actually run")

	require.NoError(t, h.mgr.SendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn settled", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 2 || hasSessionErrorNotice(events.snapshot())
	})

	assert.False(t, hasSessionErrorNotice(events.snapshot()), "the session must still be resumable")
	assert.Equal(t, 2, countAssistantReplies(h.parentMessages(id)))
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

	h := newModelAwareHarness(t, []string{"fake-model", "other-model"}, respond)

	defer h.shutdown()
	defer close(release)

	id, err := h.mgr.Send(h.ctx, h.projectID, "HOLD please", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("live session attached", func() bool { return h.liveSession(id) != nil })

	before, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)

	// The harness session has no unified config, so it refuses every switch —
	// which is exactly the "live session says no" case.
	err = h.mgr.SetModel(h.ctx, id, "other-model", "high")
	require.Error(t, err, "a refused switch must surface to the caller")

	rec, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", rec.Model)
	assert.Equal(t, before.ReasoningLevel, rec.ReasoningLevel)
}

// newModelAwareHarness is the subagent harness with an LLM client factory that
// mirrors llm.NewClient: an id outside the catalog cannot build a client.
func newModelAwareHarness(
	t *testing.T,
	known []string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedStore := schedule.NewStore(db)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: known[0]}

	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessStore, sessStore, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(c *config.Config) (llm.Client, error) {
			if !slices.Contains(known, c.Model) {
				return nil, fmt.Errorf("model %q not found in config", c.Model)
			}

			return &scriptedLLM{respond: respond}, nil
		}),
	)

	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		sessStore,
		schedule.NewService(schedStore),
		func() string { return known[0] },
	)

	for _, id := range known {
		mgr.modelCatalog = append(mgr.modelCatalog, modelInfo{ID: id})
	}

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}
}

func (h *subagentHarness) liveSession(sessionID int64) session.Service {
	rs, ok := h.mgr.runners.Load(sessionID)

	if !ok {
		return nil
	}

	return rs.Service()
}

func countAssistantReplies(msgs []llmwire.Message) int {
	count := 0

	for _, m := range msgs {
		if m.Role == llmwire.RoleAssistant && m.Content != "" {
			count++
		}
	}

	return count
}

func hasSessionErrorNotice(events []controllerapi.SessionNotification) bool {
	for _, e := range events {
		if e.Notification.Type == sessionevent.NotifyMessage &&
			strings.Contains(e.Notification.Message, "Session error") {
			return true
		}
	}

	return false
}
