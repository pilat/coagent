package hooks

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countedHookPayload struct {
	calls atomic.Int32
}

type malformedOutputCase struct {
	name         string
	stdout       string
	parseFailure bool
}

func (p *countedHookPayload) MarshalJSON() ([]byte, error) {
	p.calls.Add(1)
	return []byte(`{"encoded":true}`), nil
}

func TestPreToolUseMalformedJSONUsesExitCode(t *testing.T) {
	rows := []malformedOutputCase{
		{
			name:   "plain text",
			stdout: "plain output",
		},
		{
			name:   "unclosed brace is plain text",
			stdout: "{debug",
		},
		{
			name:         "braced invalid JSON is a parse failure",
			stdout:       "  {not json}\n",
			parseFailure: true,
		},
		{
			name:         "two objects are a parse failure",
			stdout:       "{\"decision\":\"block\"}\n{\"decision\":\"allow\"}",
			parseFailure: true,
		},
	}
	for _, row := range rows {
		for _, code := range []int{0, 2, 1} {
			for _, policy := range []string{"", "block"} {
				t.Run(row.name+"/"+strconv.Itoa(code)+"/"+policy, func(t *testing.T) {
					checkMalformedHookCase(t, row, code, policy)
				})
			}
		}
	}
}

func checkMalformedHookCase(t *testing.T, row malformedOutputCase, code int, policy string) {
	t.Helper()
	f, refusal := runMalformedHookCase(t, row.stdout, code, policy)
	if code == 2 {
		assert.Equal(t, "PreToolUse:Bash hook error: [guard]: exit code refusal", refusal)
		assert.Zero(t, f.logs.FilterMessage("hook_failed").Len())
		return
	}
	if !row.parseFailure && code == 0 {
		assert.Empty(t, refusal)
		assert.Zero(t, f.logs.FilterMessage("hook_failed").Len())
		return
	}
	logs := f.logs.FilterMessage("hook_failed").All()
	require.Len(t, logs, 1)
	if row.parseFailure {
		assert.Equal(t, "invalid JSON output", logs[0].ContextMap()["cause"])
	}
	if policy == "block" {
		assert.Contains(t, refusal, "hook failed:")
	} else {
		assert.Empty(t, refusal)
	}
}

func runMalformedHookCase(t *testing.T, stdout string, code int, policy string) (hookFixture, string) {
	t.Helper()
	f := newHookFixture(t)
	handler := commandHandler("guard")
	if policy != "" {
		handler["onFailure"] = policy
	}
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", handler))
	recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome {
		return Outcome{
			Stdout:   []byte(stdout),
			Stderr:   []byte("exit code refusal"),
			ExitCode: code,
		}
	}}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	return f, hooks.BeforeCall(f.ctx, bashCall()).Refusal
}

func TestRunMarshalsPayloadOnceBeforeFanout(t *testing.T) {
	f := newHookFixture(t)
	f.settings(
		t,
		"settings.json",
		hookSettings("PostToolUse", "Bash", commandHandler("first"), commandHandler("second")),
	)
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	payload := &countedHookPayload{}
	input := postToolPayload{
		toolPayload:  hooks.toolPayload(f.ctx, "PostToolUse", bashCall(), hookIdentity(bashCall())),
		ToolResponse: payload,
	}
	results := hooks.run(f.ctx, "PostToolUse", []string{"Bash"}, input)
	require.Len(t, results, 2)
	assert.Equal(t, int32(1), payload.calls.Load())
	require.Len(t, recorder.payloads, 2)
	assert.Equal(t, recorder.payloads[0], recorder.payloads[1])
	assert.Equal(t, map[string]any{"encoded": true}, recorder.payloads[0]["tool_response"])
}
