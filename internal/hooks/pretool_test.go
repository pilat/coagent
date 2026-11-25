package hooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreToolUseExitTwoRefusesWithStderr(t *testing.T) {
	f := newHookFixture(t)
	command := "printf 'keep the files' >&2; exit 2"
	hooks := f.command(t, commandHandler(command))
	assert.Equal(
		t,
		"PreToolUse:Bash hook error: ["+command+"]: keep the files",
		hooks.BeforeCall(f.ctx, bashCall()).Refusal,
	)
}

func TestPreToolUseJSONDenyAskDeferRefuse(t *testing.T) {
	for _, decision := range []string{"deny", "ask", "defer"} {
		t.Run(decision, func(t *testing.T) {
			f := newHookFixture(t)
			output := `{"hookSpecificOutput":{"permissionDecision":"` + decision + `","permissionDecisionReason":"needs approval"}}`
			hooks := f.command(t, commandHandler(jsonCommand(output)))
			assert.Contains(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal, "]: needs approval")
		})
	}
}

func TestPreToolUseJSONAllowAndExitZeroProceed(t *testing.T) {
	for _, command := range []string{
		"printf 'plain output'; printf 'discard me' >&2",
		jsonCommand(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`),
		jsonCommand(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`) + "; exit 1",
		jsonCommand(`{"decision":"approve"}`),
	} {
		t.Run(command, func(t *testing.T) {
			f := newHookFixture(t)
			hooks := f.command(t, commandHandler(command))
			verdict := hooks.BeforeCall(f.ctx, bashCall())
			assert.Empty(t, verdict.Refusal)
			assert.Empty(t, verdict.Context)
		})
	}
}

func TestPreToolUseLegacyDecisionBlock(t *testing.T) {
	f := newHookFixture(t)
	hooks := f.command(t, commandHandler(jsonCommand(`{"decision":"block","reason":"legacy guard"}`)))
	assert.Contains(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal, "]: legacy guard")
}

func TestPreToolUseOtherExitCodeProceedsAndLogs(t *testing.T) {
	f := newHookFixture(t)
	hooks := f.command(t, commandHandler("printf 'broken guard' >&2; exit 1"))
	assert.Empty(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal)
	assert.Equal(t, 1, f.logs.FilterMessage("hook_failed").Len())
}

func TestPreToolUseTimeoutProceeds(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("exec sleep 10")
	handler["timeout"] = 0.01
	hooks := f.command(t, handler)
	assert.Empty(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal)
	assert.Equal(t, 1, f.logs.FilterMessage("hook_failed").Len())
}

func TestPreToolUseOnFailureBlockRefuses(t *testing.T) {
	for _, command := range []string{
		"printf 'guard failed' >&2; exit 1",
		jsonCommand(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`) + "; exit 1",
	} {
		t.Run(command, func(t *testing.T) {
			f := newHookFixture(t)
			handler := commandHandler(command)
			handler["onFailure"] = "block"
			hooks := f.command(t, handler)
			assert.Contains(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal, "hook failed:")
		})
	}
}

func TestPreToolUseMissingCommandProceeds(t *testing.T) {
	f := newHookFixture(t)
	handler := commandHandler("/nonexistent/coagent-hook-test")
	handler["args"] = []string{}
	hooks := f.command(t, handler)
	assert.Empty(t, hooks.BeforeCall(f.ctx, bashCall()).Refusal)
	assert.Equal(t, 1, f.logs.FilterMessage("hook_failed").Len())
}

func TestPreToolUseCancelledContextRefuses(t *testing.T) {
	f := newHookFixture(t)
	hooks := f.command(t, commandHandler("exit 0"))
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	assert.Equal(t, "PreToolUse hook interrupted: the session is stopping", hooks.BeforeCall(ctx, bashCall()).Refusal)
}
