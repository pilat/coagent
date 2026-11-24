package daemon

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func spawnEndTarget(t *testing.T, h *harness, caller int64, prompt string) int64 {
	t.Helper()
	_, path := orchestratorProject(t, h, "end-target")
	id, _, err := (orchestratorHost{s: h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: path, Prompt: prompt,
	})
	require.NoError(t, err)
	return id
}

func endToolResult(t *testing.T, h *harness, caller, target int64, id string) (*tool.Result, error) {
	t.Helper()
	for _, candidate := range orchestrator.NewTools(orchestratorHost{s: h.mgr}, caller) {
		if candidate.ID() == id {
			return candidate.Execute(h.ctx, json.RawMessage(fmt.Sprintf(`{"session_id":%d}`, target)))
		}
	}
	t.Fatalf("missing tool %s", id)
	return nil, nil
}

func endRowCounts(t *testing.T, h *harness, target int64) [2]int {
	t.Helper()
	var counts [2]int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		(SELECT COUNT(*) FROM session_inbox WHERE session_id = ?),
		(SELECT COUNT(*) FROM session_outbox WHERE session_id = ?)`, target, target).Scan(&counts[0], &counts[1]))
	return counts
}

func runningEndTarget(t *testing.T) (*harness, int64, int64, int64) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := orchestratorProjectHarness(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BLOCK_TARGET") {
			once.Do(func() { close(entered) })
			<-release
		}
		return textReply("done")
	})
	t.Cleanup(func() { closeOnce(release) })
	h.startInboxWake()
	caller := modeRoot(t, h)
	require.NoError(t, h.store.SetSessionTitle(h.ctx, caller, "orchestrator"))
	target := spawnEndTarget(t, h, caller, "BLOCK_TARGET")
	waitForScenarioSignal(t, entered, "spawned target provider entered")
	watcher := modeRoot(t, h)
	_, _, err := (orchestratorHost{s: h.mgr}).Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	return h, caller, target, watcher
}

func assertEndCommand(t *testing.T, h *harness, caller, target int64, command, acknowledgement string) {
	t.Helper()
	var raw, attributes string
	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*), raw_content, attributes
		FROM session_inbox WHERE session_id = ? AND source = 'user'`, target).Scan(&count, &raw, &attributes))
	assert.Equal(t, 1, count)
	assert.Equal(t, command, raw)
	var attrs map[string]any
	require.NoError(t, json.Unmarshal([]byte(attributes), &attrs))
	project, err := h.store.GetProjectName(h.ctx, h.session(caller).ProjectID)
	require.NoError(t, err)
	title, _ := h.session(caller).Attributes[controllerapi.SessionAttributeTitle].(string)
	name := sessionstore.ChatDisplayName(project, title)
	assert.Equal(t, map[string]any{
		controllerapi.SessionAttributeManagerID:    "owner",
		sessionstore.InputAttributeSenderSessionID: float64(caller),
		sessionstore.InputAttributeSenderName:      name,
		sessionstore.InputAttributeSenderTitle:     title,
	}, attrs)
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == fmt.Sprintf("%s — asked by #%d %s", acknowledgement, caller, name)
	}))
}

func assertEndActorQuiet(t *testing.T, h *harness, caller, target int64) {
	t.Helper()
	assert.Zero(t, signalRows(t, h, caller, target))
	states, err := h.store.WatchStates(h.ctx, caller)
	require.NoError(t, err)
	require.Contains(t, states, target)
	assert.False(t, states[target].Armed)
	assert.False(t, states[target].Signalled)
}
