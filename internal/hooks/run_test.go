package hooks

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestHandlersRunInParallel(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", commandHandler("first"), commandHandler("second")))
	started := make(chan string, 2)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	launch := func(ctx context.Context, process Process) (Outcome, error) {
		started <- process.Argv[2]
		select {
		case <-release:
			return Outcome{ExitCode: 2, Stderr: []byte(process.Argv[2])}, nil
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	hooks, err := Load(ctx, f.dir, 42, launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	done := make(chan tool.HookVerdict, 1)
	go func() { done <- hooks.BeforeCall(ctx, bashCall()) }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("matching handlers did not both start before release")
		}
	}
	close(release)
	select {
	case verdict := <-done:
		assert.Contains(t, verdict.Refusal, "]: first")
		assert.Contains(t, verdict.Refusal, "]: second")
	case <-ctx.Done():
		t.Fatal("parallel handlers did not finish")
	}
}

func TestAsyncInvalidPayloadNeverBlocks(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("guard")
	handler["async"] = true
	handler["onFailure"] = "block"
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", handler))
	hooks := f.load(t)
	require.NotNil(t, hooks)
	assert.Empty(t, hooks.BeforeCall(f.ctx, hookCall("native", "", "{")).Refusal)
}

func TestExecFormArgs(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("${CLAUDE_PROJECT_DIR}/bin/check")
	handler["args"] = []string{"${CLAUDE_PROJECT_DIR}/.claude/check.js", "a b", "literal-$HOME"}
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", handler))
	var launched Process
	launch := func(_ context.Context, process Process) (Outcome, error) {
		launched = process
		return Outcome{}, nil
	}
	hooks, err := Load(f.ctx, f.dir, 42, launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	assert.Empty(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal)
	assert.Equal(t, []string{f.dir + "/bin/check", f.dir + "/.claude/check.js", "a b", "literal-$HOME"}, launched.Argv)
	assert.Contains(t, launched.Env, "CLAUDE_PROJECT_DIR="+f.dir)
	assert.NotEmpty(t, launched.Stdin)
}
