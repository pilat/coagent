package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/orchestrator"
)

func TestOrchestratorSpawnModel(t *testing.T) {
	for _, tc := range []struct {
		name, model, wantModel, wantEffort string
	}{
		{name: "tagged with effort", model: "fast", wantModel: "fast", wantEffort: "low"},
		{name: "tagged without effort", model: "deep", wantModel: "deep", wantEffort: "medium"},
		{name: "omitted", wantModel: "fake-model", wantEffort: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := orchestratorModelHarness(t)
			caller := modeRoot(t, h)
			id, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
				Project: h.workDir(),
				Prompt:  "do work",
				Model:   tc.model,
			})
			require.NoError(t, err)
			record := h.session(id)
			assert.Equal(t, tc.wantModel, record.Model)
			assert.Equal(t, tc.wantEffort, record.ReasoningLevel)
			h.startInboxWake()
			h.waitAnswer(id, "done")
			assertSpawnedRoot(t, h, caller, id, h.projectID, "do work")
		})
	}
}

func TestOrchestratorSpawnModelRefusals(t *testing.T) {
	h := orchestratorModelHarness(t)
	caller := modeRoot(t, h)
	for _, model := range []string{"plain", "missing", "fake-model", " fast", "inherit"} {
		t.Run(model, func(t *testing.T) {
			before := orchestratorModelCounts(t, h)
			_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
				Project:  "nope",
				Worktree: "login",
				Prompt:   "do work",
				Model:    model,
			})
			require.EqualError(t, err, fmt.Sprintf(
				"model %q is not an advertised tagged candidate; choose one of: fast, deep, or omit model for the daemon's default model",
				model,
			))
			assert.Equal(t, before, orchestratorModelCounts(t, h))
		})
	}
}

func TestOrchestratorSpawnModelWithoutCandidates(t *testing.T) {
	h := orchestratorProjectHarness(t)
	caller := modeRoot(t, h)
	before := orchestratorSessionCount(t, h)
	_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: "nope",
		Prompt:  "do work",
		Model:   "fast",
	})
	require.EqualError(
		t,
		err,
		`model "fast" is not an advertised tagged candidate; no tagged models are configured, so omit model for the daemon's default model`,
	)
	assert.Equal(t, before, orchestratorSessionCount(t, h))
}

func orchestratorModelHarness(t *testing.T) *harness {
	t.Helper()
	root, worktrees := t.TempDir(), t.TempDir()
	return newHarness(t, harnessOptions{
		respond: trivialRespond,
		configure: func(cfg *config.Config) {
			cfg.WorkDir = filepath.Join(root, "initial")
			require.NoError(t, os.MkdirAll(cfg.WorkDir, 0o755))
			cfg.UnifiedConfig = &config.UnifiedConfig{
				ProjectsRoot:  root + "/.",
				WorktreesRoot: worktrees,
				Models: []config.ModelEntry{
					{
						ID:            "fake-model",
						ContextWindow: 200000,
						Reasoning: &config.ReasoningSpec{
							Supported: true, NativeEffort: true, Efforts: []string{"low", "high"},
						},
						EffortLevels:  []string{"low", "high"},
						DefaultEffort: "high",
					},
					{
						ID:            "fast",
						Tags:          []string{"fast"},
						ContextWindow: 200000,
						Reasoning: &config.ReasoningSpec{
							Supported: true, NativeEffort: true, Efforts: []string{"low", "medium"},
						},
						EffortLevels:  []string{"low", "medium"},
						DefaultEffort: "low",
					},
					{
						ID:            "deep",
						Tags:          []string{"deep"},
						ContextWindow: 200000,
					},
					{
						ID:            "plain",
						ContextWindow: 200000,
						Reasoning: &config.ReasoningSpec{
							Supported: true, NativeEffort: true, Efforts: []string{"low"},
						},
						EffortLevels:  []string{"low"},
						DefaultEffort: "low",
					},
				},
			}
		},
	})
}

func orchestratorModelCounts(t *testing.T, h *harness) [3]int {
	t.Helper()
	var sessions, projects, watches int
	require.NoError(t, h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM sessions").Scan(&sessions))
	require.NoError(t, h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM projects").Scan(&projects))
	require.NoError(t, h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM session_watches").Scan(&watches))
	return [3]int{sessions, projects, watches}
}
