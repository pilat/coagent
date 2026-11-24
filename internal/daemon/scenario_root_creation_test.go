package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/subagent"
)

func TestHumanWorktreeIsCreatedByTheDaemonPath(t *testing.T) {
	worktrees := t.TempDir()
	clone := orchestratorClone(t)
	h := newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
		cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: t.TempDir(), WorktreesRoot: worktrees}
	}})
	_, err := h.store.GetOrCreateProject(h.ctx, clone)
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "executed")
	script := fmt.Sprintf("#!/bin/sh\nprintf called >> %q\n", marker)
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "hooks", "post-checkout"), []byte(script), 0o755))
	id, err := newTestController(h.mgr).ForManager("telegram").CreateSession(h.ctx, controllerapi.SessionCreateData{
		WorkDir: clone, WorktreeName: "api", Attributes: map[string]any{"channel": "telegram"},
	})
	require.NoError(t, err)
	path := projectpath.WorktreePath(worktrees, clone, "api")
	require.DirExists(t, path)
	assert.FileExists(t, marker)
	record := h.session(id)
	assert.Equal(t, clone, record.Attributes["repo_root"])
	assert.Equal(
		t,
		controllerapi.WorktreeOriginController,
		record.Attributes[controllerapi.SessionAttributeWorktreeOrigin],
	)
	assert.NotContains(t, record.Attributes, "manager_id")
	assert.Equal(t, "telegram", record.Attributes["channel"])
	assert.Nil(t, record.KilledAt)
	name, err := h.store.GetProjectName(h.ctx, record.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, projectpath.RepoNamespace(clone)+"/api", name)
	workDir, err := h.store.GetProjectWorkDir(h.ctx, record.ProjectID)
	require.NoError(t, err)
	assert.Equal(t, path, workDir)
	var openedName, openedDir string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `
		SELECT json_extract(attributes,'$.name'), json_extract(attributes,'$.work_dir') FROM session_outbox
		WHERE session_id = ? AND type = 'session_opened'`, id).Scan(&openedName, &openedDir))
	assert.Equal(t, name, openedName)
	assert.Equal(t, path, openedDir)
	var rows int
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		"SELECT count(*) FROM session_outbox WHERE session_id = ? AND type = 'session_opened'", id).Scan(&rows))
	assert.Equal(t, 1, rows)
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		"SELECT count(*) FROM session_inbox WHERE session_id = ?", id).Scan(&rows))
	assert.Zero(t, rows)
}

func TestHumanWorktreeRollsBackWhenItsRootCannotStart(t *testing.T) {
	worktrees := t.TempDir()
	clone := orchestratorClone(t)
	h := newHarness(t, harnessOptions{
		respond: trivialRespond,
		links:   func(inner subagent.Store) subagent.Store { return failingLinkReads{Store: inner} },
		configure: func(cfg *config.Config) {
			cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: t.TempDir(), WorktreesRoot: worktrees}
		},
	})
	_, err := h.store.GetOrCreateProject(h.ctx, clone)
	require.NoError(t, err)
	id, err := newTestController(h.mgr).ForManager("telegram").CreateSession(h.ctx, controllerapi.SessionCreateData{
		WorkDir: clone, WorktreeName: "api", Attributes: map[string]any{"channel": "telegram"},
	})
	require.ErrorIs(t, err, errLinkRead)
	assert.Zero(t, id)
	var killed bool
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		"SELECT killed_at IS NOT NULL FROM sessions ORDER BY id DESC LIMIT 1").Scan(&killed))
	assert.True(t, killed)
	assert.NoDirExists(t, projectpath.WorktreePath(worktrees, clone, "api"))
	exists, err := git.NewWorktreeClient().BranchExists(h.ctx, clone, "api")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestRootCreationRefusalsStayExact(t *testing.T) {
	for _, test := range []struct {
		name     string
		human    bool
		worktree string
		branch   bool
		ssh      bool
		want     string
	}{
		{
			name: "human invalid name", human: true, worktree: "bad:name",
			want: `worktree: worktree name: project name must not contain "/", "\", ":", or a NUL byte`,
		},
		{
			name: "human existing branch", human: true, worktree: "api", branch: true,
			want: `worktree: branch "api" already exists`,
		},
		{
			name: "human reserved project", human: true,
			want: `resolve project: get project: resolve project: project directory name "a:b" is reserved`,
		},
		{
			name: "spawn invalid name", worktree: "bad:name",
			want: `worktree: worktree name: project name must not contain "/", "\", ":", or a NUL byte`,
		},
		{
			name: "spawn existing branch", worktree: "api", branch: true,
			want: `worktree: branch "api" already exists`,
		},
		{
			name: "spawn executable config", worktree: "api", ssh: true,
			want: `worktree: check worktree source: repository config defines core.sshcommand; create the worktree with /gwt`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktrees := t.TempDir()
			clone := orchestratorClone(t)
			h := newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
				cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: t.TempDir(), WorktreesRoot: worktrees}
			}})
			caller := modeRoot(t, h)
			_, err := h.store.GetOrCreateProject(h.ctx, clone)
			require.NoError(t, err)
			if test.branch {
				orchestratorGit(t, clone, "branch", "api")
			}
			if test.ssh {
				orchestratorGit(t, clone, "config", "core.sshCommand", "/bin/true")
			}
			workDir := clone
			path := projectpath.WorktreePath(worktrees, clone, test.worktree)
			if test.worktree == "" {
				workDir = filepath.Join(t.TempDir(), "a:b")
				require.NoError(t, os.MkdirAll(workDir, 0o755))
				path = workDir
			}
			before := orchestratorSessionCount(t, h)
			if test.human {
				_, err = newTestController(
					h.mgr,
				).ForManager("telegram").
					CreateSession(h.ctx, controllerapi.SessionCreateData{
						WorkDir:      workDir,
						WorktreeName: test.worktree,
						Attributes:   map[string]any{"channel": "telegram"},
					})
			} else {
				_, _, err = (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
					Project: clone, Worktree: test.worktree, Prompt: "do work",
				})
			}
			require.EqualError(t, err, test.want)
			assert.Equal(t, before, orchestratorSessionCount(t, h))
			if test.worktree != "" {
				assert.NoDirExists(t, path)
			}
			var rows int
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				"SELECT count(*) FROM projects WHERE work_dir = ?", path).Scan(&rows))
			assert.Zero(t, rows)
			if test.branch {
				exists, err := git.NewWorktreeClient().BranchExists(h.ctx, clone, "api")
				require.NoError(t, err)
				assert.True(t, exists)
			}
		})
	}
}

func TestRootFirstInputStartsTheEpisodeAsBefore(t *testing.T) {
	for _, test := range []struct {
		prompt  string
		started bool
	}{
		{prompt: "fix login 500", started: true},
		{prompt: "/compactx", started: true},
		{prompt: "/status"},
		{prompt: "  /status  "},
		{prompt: "/help"},
		{prompt: "/schedules"},
		{prompt: "/compact"},
		{prompt: "/compact keep the tests"},
		{prompt: "/stop"},
		{prompt: "/clear"},
		{prompt: "/kill"},
	} {
		t.Run(test.prompt, func(t *testing.T) {
			worktrees := t.TempDir()
			h := newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
				cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: t.TempDir(), WorktreesRoot: worktrees}
			}})
			id, err := h.mgr.Send(h.ctx, h.projectID, test.prompt, "fake-model", nil)
			require.NoError(t, err)
			var started bool
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				"SELECT episode_started_at IS NOT NULL FROM sessions WHERE id = ?", id).Scan(&started))
			assert.Equal(t, test.started, started)
			var source, raw string
			require.NoError(t, h.db.QueryRowContext(h.ctx, `
				SELECT source, raw_content FROM session_inbox
				WHERE session_id = ? ORDER BY id LIMIT 1`, id).Scan(&source, &raw))
			assert.Equal(t, "user", source)
			assert.Equal(t, test.prompt, raw)
			if test.prompt == "fix login 500" {
				assert.Equal(t, test.prompt, h.session(id).Attributes[controllerapi.SessionAttributeTitle])
			} else if !test.started {
				assert.NotContains(t, h.session(id).Attributes, controllerapi.SessionAttributeTitle)
			}
		})
	}
}
