package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestLoadMergesGlobalProjectAndLocal(t *testing.T) {
	f := newHookFixture(t)
	for _, location := range []string{"global", "settings.json", "settings.local.json"} {
		handler := commandHandler("printf '%s' " + shellQuote(location) + " >&2; exit 2")
		f.settings(t, location, hookSettings("PreToolUse", "*", handler))
	}
	hooks := f.load(t)
	require.NotNil(t, hooks)
	verdict := hooks.BeforeCall(f.ctx, bashCall())
	global := strings.Index(verdict.Refusal, "]: global")
	project := strings.Index(verdict.Refusal, "]: settings.json")
	local := strings.Index(verdict.Refusal, "]: settings.local.json")
	assert.GreaterOrEqual(t, global, 0)
	assert.Greater(t, project, global)
	assert.Greater(t, local, project)
}

func TestLoadNeverReadsClaudeHomeSettings(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "claude-home", hookSettings("PreToolUse", "*", commandHandler("exit 2")))
	assert.Nil(t, f.load(t))
}

func TestLoadDisableAllHooksScope(t *testing.T) {
	tests := []struct {
		name     string
		global   bool
		project  bool
		local    *bool
		want     []string
		disabled string
	}{
		{
			name:     "global true cannot be overridden",
			global:   true,
			local:    new(false),
			disabled: "global",
		},
		{
			name:     "project true keeps global",
			project:  true,
			want:     []string{"global"},
			disabled: "settings.json",
		},
		{
			name:    "local false overrides project true",
			project: true,
			local:   new(false),
			want:    []string{"global", "settings.json", "settings.local.json"},
		},
		{
			name:     "local true drops both project files",
			local:    new(true),
			want:     []string{"global"},
			disabled: "settings.local.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHookFixture(t)
			for _, location := range []string{"global", "settings.json", "settings.local.json"} {
				settings := hookSettings("PreToolUse", "*", commandHandler(location))
				switch location {
				case "global":
					settings["disableAllHooks"] = tt.global
				case "settings.json":
					settings["disableAllHooks"] = tt.project
				case "settings.local.json":
					if tt.local != nil {
						settings["disableAllHooks"] = *tt.local
					}
				}
				f.settings(t, location, settings)
			}
			var calls []string
			var mu sync.Mutex
			launch := func(_ context.Context, process Process) (Outcome, error) {
				mu.Lock()
				calls = append(calls, process.Argv[2])
				mu.Unlock()
				return Outcome{}, nil
			}
			hooks, err := Load(f.ctx, f.dir, 42, launch)
			require.NoError(t, err)
			if len(tt.want) == 0 {
				assert.Nil(t, hooks)
			} else {
				require.NotNil(t, hooks)
				assert.Empty(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal)
				assert.ElementsMatch(t, tt.want, calls)
			}
			logs := f.logs.FilterMessage("hooks_disabled").All()
			if tt.disabled == "" {
				assert.Empty(t, logs)
				return
			}
			require.Len(t, logs, 1)
			path := filepath.Join(f.dir, ".claude", tt.disabled)
			if tt.disabled == "global" {
				path = filepath.Join(f.home, ".coagent", "settings.json")
			}
			assert.Equal(t, path, logs[0].ContextMap()["path"])
			assert.Equal(t, zap.InfoLevel, logs[0].Level)
		})
	}
}

func TestLoadInvalidJSONContributesNothing(t *testing.T) {
	f := newHookFixture(t)
	path := filepath.Join(f.dir, ".claude", "settings.json")
	writeHookFile(t, path, []byte(`{"hooks":`))
	f.settings(t, "settings.local.json", hookSettings("PreToolUse", "*", commandHandler("exit 2")))
	assert.NotNil(t, f.load(t))
	entries := f.logs.FilterMessage("hooks_settings_invalid").All()
	require.Len(t, entries, 1)
	assert.Equal(t, path, entries[0].ContextMap()["path"])
	assert.NotEmpty(t, entries[0].ContextMap()["error"])
}

func TestLoadSkipsProjectFilesWhenProjectIsHome(t *testing.T) {
	for _, name := range []string{"actual home", "home symlink alias", "trailing slash home"} {
		t.Run(name, func(t *testing.T) {
			f := newHookFixture(t)
			canonical, err := filepath.EvalSymlinks(f.home)
			require.NoError(t, err)
			f.dir = canonical
			switch name {
			case "home symlink alias":
				alias := filepath.Join(t.TempDir(), "home-alias")
				require.NoError(t, os.Symlink(f.home, alias))
				t.Setenv("HOME", alias)
			case "trailing slash home":
				t.Setenv("HOME", f.home+string(os.PathSeparator))
			}
			f.settings(t, "settings.json", hookSettings("PreToolUse", "*", commandHandler("exit 2")))
			f.settings(t, "settings.local.json", hookSettings("PreToolUse", "*", commandHandler("exit 2")))
			assert.Nil(t, f.load(t))
			assert.Equal(t, 1, f.logs.FilterMessage("hooks_project_settings_skipped").Len())
			f.settings(t, "global", hookSettings("PreToolUse", "*", commandHandler("printf global >&2; exit 2")))
			hooks := f.load(t)
			require.NotNil(t, hooks)
			verdict := hooks.BeforeCall(f.ctx, bashCall())
			assert.Contains(t, verdict.Refusal, "]: global")
			assert.Equal(t, 1, strings.Count(verdict.Refusal, "hook error:"))
			assert.Equal(t, 2, f.logs.FilterMessage("hooks_project_settings_skipped").Len())
		})
	}
}

func TestLoadUnsupportedHandlersWarnOncePerEvent(t *testing.T) {
	f := newHookFixture(t)
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop"} {
		f.settings(t, "settings.json", hookSettings(event, "*",
			map[string]any{"type": "prompt"}, map[string]any{"type": "http"}))
		assert.Nil(t, f.load(t))
		logs := f.logs.FilterMessage("hooks_handler_unsupported").FilterField(zap.String("event", event)).All()
		require.Len(t, logs, 1)
		assert.Equal(t, zap.WarnLevel, logs[0].Level)
		assert.Equal(t, "prompt", logs[0].ContextMap()["type"])
		assert.Equal(t, filepath.Join(f.dir, ".claude", "settings.json"), logs[0].ContextMap()["path"])
	}
}

func TestLoadIgnoresProjectSymlinkOutsideProject(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "global", hookSettings("PreToolUse", "*", commandHandler("exit 2")))
	outside := filepath.Join(t.TempDir(), "settings.json")
	writeHookFile(
		t,
		outside,
		[]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"printf outside >&2; exit 2"}]}]}}`),
	)
	project := filepath.Join(f.dir, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(project), 0o700))
	require.NoError(t, os.Symlink(outside, project))
	hooks := f.load(t)
	require.NotNil(t, hooks)
	verdict := hooks.BeforeCall(f.ctx, bashCall())
	assert.Equal(t, 1, strings.Count(verdict.Refusal, "hook error:"))
	assert.NotContains(t, verdict.Refusal, "outside")
}

func TestLoadNoHandlersReturnsNil(t *testing.T) {
	f := newHookFixture(t)
	assert.Nil(t, f.load(t))
	f.settings(t, "settings.json", map[string]any{"hooks": map[string]any{}})
	assert.Nil(t, f.load(t))
	f.settings(t, "settings.json", hookSettings("SessionStart", "*", commandHandler("exit 2")))
	assert.Nil(t, f.load(t))
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", map[string]any{"type": "prompt", "prompt": "deny"}))
	assert.Nil(t, f.load(t))
}

func TestLoadIfFieldRunsHandlerAndWarns(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("printf 'still runs' >&2; exit 2")
	handler["if"] = "false"
	hooks := f.command(t, handler)
	assert.Contains(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal, "still runs")
	entries := f.logs.FilterMessage("hooks_if_ignored").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "false", entries[0].ContextMap()["if"])
}

func TestLoadDeduplicatesIdenticalHandlers(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("printf 'one copy' >&2; exit 2")
	for _, location := range []string{"global", "settings.json", "settings.local.json"} {
		f.settings(t, location, hookSettings("PreToolUse", "*", handler))
	}
	hooks := f.load(t)
	require.NotNil(t, hooks)
	assert.Equal(t, 1, strings.Count(hooks.BeforeCall(f.ctx, bashCall()).Refusal, "hook error:"))
}

func TestMatcherExactListRegexAndAll(t *testing.T) {
	tests := []struct {
		matcher string
		name    string
		matches bool
	}{
		{matcher: "Bash", name: "Bash", matches: true},
		{matcher: "Edit|Write", name: "Write", matches: true},
		{matcher: "Edit, Write", name: "Write", matches: true},
		{matcher: "*", name: "anything", matches: true},
		{matcher: "", name: "anything", matches: true},
		{matcher: "mcp__memory__.*", name: "mcp__memory__read", matches: true},
		{matcher: "Edit.*", name: "NotebookEditCell", matches: true},
		{matcher: "Bash", name: "bash", matches: false},
		{matcher: "(?=Bash)", name: "Bash", matches: false},
	}
	for _, tt := range tests {
		t.Run(tt.matcher+"/"+tt.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.settings(
				t,
				"settings.json",
				hookSettings("PreToolUse", tt.matcher, commandHandler("printf 'matched' >&2; exit 2")),
			)
			hooks := f.load(t)
			if hooks == nil {
				assert.False(t, tt.matches)
				return
			}
			assert.Equal(t, tt.matches, hooks.BeforeCall(f.ctx, hookCall("native", tt.name, `{}`)).Refusal != "")
		})
	}
}
