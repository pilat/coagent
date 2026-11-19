package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestOrchestratorListSessions(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		once.Do(func() { close(entered) })
		<-release
		return textReply("done")
	}})
	defer closeOnce(release)
	h.startInboxWake()
	caller := modeRoot(t, h)
	idle := h.createRoot(managerAttrs("owner"))
	require.NoError(t, h.store.SetSessionTitle(h.ctx, "owner", idle, "needle"))
	stopped := h.createRoot(managerAttrs("owner"))
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, stopped, sessionstore.SessionStatusStopped))
	parked := h.createRoot(managerAttrs("owner"))
	fireOrchestratorBudget(t, h, parked)
	foreign := h.createRoot(managerAttrs("foreign"))
	child := h.createUnlinkedChild(caller)
	running := h.send("hold", managerAttrs("owner"))
	waitForScenarioSignal(t, entered, "running root")
	entries, total, err := (orchestratorHost{h.mgr}).ListSessions(h.ctx, caller, "")
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	states := make(map[int64]string)
	for _, entry := range entries {
		states[entry.ID] = entry.State
		assert.False(t, entry.Activity.IsZero())
		assert.Equal(t, entry.ID == caller, entry.Self)
	}
	assert.Equal(
		t,
		map[int64]string{caller: "idle", idle: "idle", stopped: "stopped", parked: "parked", running: "running"},
		states,
	)
	assert.NotContains(t, states, foreign)
	assert.NotContains(t, states, child)
	filtered, matched, err := (orchestratorHost{h.mgr}).ListSessions(h.ctx, caller, "NEEDLE")
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, 1, matched)
	assert.Equal(t, idle, filtered[0].ID)
	for range 16 {
		h.createRoot(managerAttrs("owner"))
	}
	entries, total, err = (orchestratorHost{h.mgr}).ListSessions(h.ctx, caller, "")
	require.NoError(t, err)
	assert.Len(t, entries, 20)
	assert.Equal(t, 21, total)
}

func TestOrchestratorSpawn(t *testing.T) {
	h := orchestratorProjectHarness(t)
	h.startInboxWake()
	caller := modeRoot(t, h)
	project, path := orchestratorProject(t, h, "named")
	require.NoError(t, h.store.SetChatFocus(h.ctx, "owner", "chat", caller))
	require.NoError(t, h.store.BindChat(h.ctx, "owner", "empty"))
	for _, selection := range []string{"NaMeD", path} {
		id, where, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
			Project: selection,
			Prompt:  "fix the login bug",
		})
		require.NoError(t, err)
		assert.Equal(t, "named", where)
		h.waitAnswer(id, "done")
		assertSpawnedRoot(t, h, caller, id, project, "fix the login bug")
	}
	focus, cursor, entered, err := h.store.ChatCursor(h.ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, caller, focus)
	assert.Zero(t, cursor)
	assert.False(t, entered)
	focus, err = h.store.ChatFocus(h.ctx, "owner", "empty")
	require.NoError(t, err)
	assert.Zero(t, focus)
	hidden, hiddenPath := orchestratorProject(t, h, "hidden")
	_, err = h.db.ExecContext(h.ctx, "UPDATE projects SET hidden = 1 WHERE id = ?", hidden)
	require.NoError(t, err)
	known, err := h.store.GetProjectName(h.ctx, h.projectID)
	require.NoError(t, err)
	names := []string{known, "named"}
	slices.Sort(names)
	worktree := projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(h.mgr.build.Config.UnifiedConfig),
		path,
		"existing",
	)
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	_, err = h.store.GetOrCreateNamedProject(h.ctx, worktree, "named/existing")
	require.NoError(t, err)
	stale := filepath.Join(t.TempDir(), "stale")
	_, err = h.store.GetOrCreateNamedProject(h.ctx, stale, "stale")
	require.NoError(t, err)
	for _, selection := range []string{"missing", "hidden", hiddenPath} {
		before := orchestratorSessionCount(t, h)
		_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
			Project: selection,
			Prompt:  "do work",
		})
		require.EqualError(
			t,
			err,
			fmt.Sprintf("no project named %q; known projects: %s", selection, strings.Join(names, ", ")),
		)
		assert.Equal(t, before, orchestratorSessionCount(t, h))
	}
	for _, selection := range []string{"named/existing", worktree} {
		_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
			Project: selection,
			Prompt:  "do work",
		})
		require.EqualError(t, err, fmt.Sprintf(
			"project %q is a worktree; name its base project and pass worktree to create a fresh one", selection))
	}
	for _, selection := range []string{"stale", stale} {
		_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
			Project: selection,
			Prompt:  "do work",
		})
		require.EqualError(t, err, fmt.Sprintf(
			"project %q directory is missing: %s; open an existing project instead", selection, stale))
	}
	_, first := orchestratorProject(t, h, "api")
	_, second := orchestratorProject(t, h, "api")
	paths := []string{first, second}
	slices.Sort(paths)
	_, _, err = (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: "api",
		Prompt:  "do work",
	})
	require.EqualError(t, err, `project name "api" is ambiguous; use one of: `+strings.Join(paths, ", "))
}

func TestOrchestratorSpawnCapacity(t *testing.T) {
	h := orchestratorProjectHarness(t)
	h.startInboxWake()
	caller := modeRoot(t, h)
	path, err := h.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	held := 0
	t.Cleanup(func() {
		for held > 0 {
			h.mgr.runners.release(false, 0)
			held--
		}
	})
	for range maxTotal - spawnHeadroom + 1 {
		require.True(t, h.mgr.runners.tryAdmit(false, 0))
		held++
	}
	before := orchestratorSessionCount(t, h)
	_, _, err = (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: path,
		Prompt:  "new work",
	})
	require.EqualError(
		t,
		err,
		fmt.Sprintf(
			"daemon is near its session capacity (%d of %d running); try again after some sessions finish",
			held,
			maxTotal,
		),
	)
	assert.Equal(t, before, orchestratorSessionCount(t, h))
	h.mgr.runners.release(false, 0)
	held--
	id, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: path,
		Prompt:  "new work",
	})
	require.NoError(t, err)
	h.waitAnswer(id, "done")
	assert.Equal(t, before+1, orchestratorSessionCount(t, h))
}

func assertSpawnedRoot(t *testing.T, h *harness, caller, id, project int64, prompt string) {
	t.Helper()
	record := h.session(id)
	assert.Equal(t, project, record.ProjectID)
	assert.Equal(t, "owner", record.Attributes[controllerapi.SessionAttributeManagerID])
	assert.Equal(t, prompt, record.Attributes[controllerapi.SessionAttributeTitle])
	assert.NotContains(t, record.Attributes, controllerapi.SessionAttributeOrchestrator)
	assert.Zero(t, record.ParentID)
	assert.Zero(t, record.RootID)
	assert.Nil(t, h.link(id))
	var source, raw, attrs string
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, "SELECT source,raw_content,attributes FROM session_inbox WHERE session_id = ?", id).
			Scan(&source, &raw, &attrs),
	)
	assert.Equal(t, "agent", source)
	assert.Equal(t, prompt, raw)
	var attributes map[string]any
	require.NoError(t, json.Unmarshal([]byte(attrs), &attributes))
	assert.InDelta(t, float64(caller), attributes[sessionstore.InputAttributeSenderSessionID], 0)
	assert.NotEmpty(t, attributes[sessionstore.InputAttributeSenderName])
	assert.Contains(t, attributes, sessionstore.InputAttributeSenderTitle)
	var rows int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM session_inbox WHERE session_id = ?", id).Scan(&rows),
	)
	assert.Equal(t, 1, rows)
}

func orchestratorProject(t *testing.T, h *harness, name string) (int64, string) {
	t.Helper()
	path := filepath.Join(projectpath.ResolveRoot(h.mgr.build.Config.UnifiedConfig), filepath.Base(t.TempDir()))
	require.NoError(t, os.MkdirAll(path, 0o755))
	id, err := h.store.GetOrCreateNamedProject(h.ctx, path, name)
	require.NoError(t, err)
	return id, path
}

func orchestratorProjectHarness(t *testing.T) *harness {
	t.Helper()
	root, worktrees := t.TempDir(), t.TempDir()
	return newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
		cfg.WorkDir = filepath.Join(root, "initial")
		require.NoError(t, os.MkdirAll(cfg.WorkDir, 0o755))
		cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: root + "/.", WorktreesRoot: worktrees}
	}})
}

func orchestratorSessionCount(t *testing.T, h *harness) int {
	t.Helper()
	var rows int
	require.NoError(t, h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM sessions").Scan(&rows))
	return rows
}
