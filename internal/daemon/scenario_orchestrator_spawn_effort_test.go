package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/projectpath"
)

func TestOrchestratorSpawnEffort(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort, wantModel string
	}{
		{name: "named model", model: "fast", effort: "high", wantModel: "fast"},
		{name: "default model", effort: "low", wantModel: "lead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newSpawnEffortProvider(t)
			h := orchestratorEffortHarness(t, provider.url)
			caller, err := h.store.CreateSession(h.ctx, h.projectID, "caller-model", "", managerAttrs("owner"))
			require.NoError(t, err)
			_, err = h.store.SetOrchestrator(h.ctx, "owner", caller.ID, true)
			require.NoError(t, err)
			id, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller.ID, orchestrator.SpawnRequest{
				Project: h.workDir(),
				Prompt:  "do work",
				Model:   tc.model,
				Effort:  tc.effort,
			})
			require.NoError(t, err)
			record := h.session(id)
			assert.Equal(t, tc.wantModel, record.Model)
			assert.Equal(t, tc.effort, record.ReasoningLevel)
			h.startInboxWake()
			h.waitAnswer(id, "done")
			assert.Equal(t, tc.effort, provider.effortFor(tc.wantModel))
			assertSpawnedRoot(t, h, caller.ID, id, h.projectID, "do work")
		})
	}
}

func TestOrchestratorSpawnEffortRefusals(t *testing.T) {
	const fastRefusal = `model fast does not accept reasoning level "high" (accepts: low, medium)`
	h := orchestratorModelHarness(t)
	caller := modeRoot(t, h)
	for _, tc := range []struct {
		name, model, effort, want string
		capacity                  bool
	}{
		{name: "named model", model: "fast", effort: "high", want: fastRefusal},
		{
			name: "default model", effort: "medium",
			want: `model fake-model does not accept reasoning level "medium" (accepts: low, high)`,
		},
		{
			name: "no levels", model: "deep", effort: "low",
			want: "model deep accepts no reasoning level; omit effort",
		},
		{
			name: "untrimmed", model: "fast", effort: " low",
			want: `model fast does not accept reasoning level " low" (accepts: low, medium)`,
		},
		{
			name: "model checked first", model: "plain", effort: "ultra",
			want: `model "plain" is not an advertised tagged candidate; choose one of: fast, deep, or omit model for the daemon's default model`,
		},
		{name: "capacity", model: "fast", effort: "high", want: fastRefusal, capacity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.capacity {
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
			}
			before := orchestratorModelCounts(t, h)
			_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
				Project:  h.workDir(),
				Worktree: "login",
				Prompt:   "do work",
				Model:    tc.model,
				Effort:   tc.effort,
			})
			require.EqualError(t, err, tc.want)
			assert.Equal(t, before, orchestratorModelCounts(t, h))
			entries, err := os.ReadDir(projectpath.ResolveWorktreesRoot(h.mgr.build.Config.UnifiedConfig))
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func orchestratorEffortHarness(t *testing.T, baseURL string) *harness {
	t.Helper()
	root, worktrees := t.TempDir(), t.TempDir()
	configure := func(cfg *config.Config) {
		cfg.WorkDir = filepath.Join(root, "initial")
		require.NoError(t, os.MkdirAll(cfg.WorkDir, 0o755))
		fast := reasoningModelEntry("fast", []string{"low", "high"}, "low")
		fast.Tags = []string{"fast"}
		cfg.UnifiedConfig = &config.UnifiedConfig{
			ProjectsRoot:  root + "/.",
			WorktreesRoot: worktrees,
			Providers: map[string]config.ProviderEntry{
				"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
			},
			Models: []config.ModelEntry{
				reasoningModelEntry("lead", []string{"low", "high"}, "high"),
				fast,
				reasoningModelEntry("caller-model", []string{"low", "high"}, "high"),
			},
		}
	}
	return newHarness(t, harnessOptions{configure: configure, clientFor: configuredClient(configure)})
}
