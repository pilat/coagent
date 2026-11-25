package hooks

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopExitTwoBlocks(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("Stop", "*", commandHandler("printf 'finish the checks' >&2; exit 2")))
	hooks := f.load(t)
	require.NotNil(t, hooks)
	reason, err := hooks.Stop(f.ctx, "candidate answer", false)
	require.NoError(t, err)
	assert.Equal(t, "finish the checks", reason)
}

func TestStopJSONBlockAndContinueFalse(t *testing.T) {
	tests := []struct {
		name    string
		outcome Outcome
		want    string
	}{
		{
			name:    "block",
			outcome: Outcome{Stdout: []byte(`{"decision":"block","reason":"keep working"}`)},
			want:    "keep working",
		},
		{
			name:    "context",
			outcome: Outcome{Stdout: []byte(`{"hookSpecificOutput":{"additionalContext":"check details"}}`)},
			want:    "check details",
		},
		{
			name: "stop wins",
			outcome: Outcome{
				ExitCode: 2,
				Stderr:   []byte("stderr reason"),
				Stdout: []byte(
					`{"continue":false,"decision":"block","reason":"block reason","hookSpecificOutput":{"additionalContext":"context reason"}}`,
				),
			},
		},
		{name: "exit one", outcome: Outcome{ExitCode: 1, Stderr: []byte("failed")}},
		{name: "plain output", outcome: Outcome{Stdout: []byte("plain output")}},
		{name: "invalid json", outcome: Outcome{Stdout: []byte(`{"decision":`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.settings(t, "settings.json", hookSettings("Stop", "*", commandHandler("guard")))
			recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome { return tt.outcome }}
			hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
			require.NoError(t, err)
			require.NotNil(t, hooks)
			reason, err := hooks.Stop(f.ctx, "candidate answer", false)
			require.NoError(t, err)
			assert.Equal(t, tt.want, reason)
		})
	}
}

func TestStopMissingFileStderrIsNonBlocking(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		wantFailure bool
	}{
		{
			name:        "single interpreter diagnostic",
			stderr:      "  python3: can't open file '/x/missing.py': [Errno 2] No such file or directory\n",
			wantFailure: true,
		},
		{
			name:   "multiline test failure",
			stderr: "FAIL test_load\nFileNotFoundError: [Errno 2] No such file or directory\n",
		},
		{
			name:   "multiline compiler failure",
			stderr: "compiling main.c\nfatal error: x.h: No such file or directory\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.settings(t, "settings.json", hookSettings("Stop", "*", commandHandler("guard")))
			recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome {
				return Outcome{
					ExitCode: 2,
					Stderr:   []byte(tt.stderr),
				}
			}}
			hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
			require.NoError(t, err)
			reason, err := hooks.Stop(f.ctx, "candidate answer", false)
			require.NoError(t, err)
			if tt.wantFailure {
				assert.Empty(t, reason)
				assert.Equal(t, 1, f.logs.FilterMessage("hook_failed").Len())
			} else {
				assert.Equal(t, strings.TrimSpace(tt.stderr), reason)
				assert.Zero(t, f.logs.FilterMessage("hook_failed").Len())
			}
		})
	}
}

func TestStopPayloadFields(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("Stop", "*", commandHandler("guard")))
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	for _, active := range []bool{false, true} {
		reason, stopErr := hooks.Stop(f.ctx, "the published answer", active)
		require.NoError(t, stopErr)
		assert.Empty(t, reason)
		payload := recorder.payloads[len(recorder.payloads)-1]
		assert.Equal(t, map[string]any{
			"session_id": "42", "cwd": f.dir, "hook_event_name": "Stop",
			"stop_hook_active": active, "last_assistant_message": "the published answer",
		}, payload)
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		reason, stopErr := hooks.Stop(ctx, "the published answer", false)
		assert.Empty(t, reason)
		require.ErrorIs(t, stopErr, context.Canceled)
	})
}

func TestStopIgnoresMatcher(t *testing.T) {
	f := newHookFixture(t)
	f.settings(
		t,
		"settings.json",
		hookSettings("Stop", "(?=invalid)", commandHandler("printf 'still checked' >&2; exit 2")),
	)
	hooks := f.load(t)
	require.NotNil(t, hooks)
	reason, err := hooks.Stop(f.ctx, "candidate answer", false)
	require.NoError(t, err)
	assert.Equal(t, "still checked", reason)
	assert.Zero(t, f.logs.FilterMessage("hooks_matcher_invalid").Len())
}
