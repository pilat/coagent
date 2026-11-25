package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type emptyHookProjector struct {
	*hookTool
	projections int
	calls       []tool.HookCall
	err         error
}

func (p *emptyHookProjector) HookCalls(context.Context, json.RawMessage) ([]tool.HookCall, error) {
	p.projections++
	return p.calls, p.err
}

func TestInvoke_ProjectionErrorKeepsNativeIdentity(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", commandHandler("guard")))
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	candidate := &emptyHookProjector{
		hookTool: &hookTool{id: "apply_patch"},
		calls: []tool.HookCall{{
			ToolID: "edit",
			Name:   tool.HookName{Name: "Edit"},
			Input:  json.RawMessage(`{}`),
		}},
		err: errors.New("projection failed"),
	}
	report := tool.Schedule(tool.WithCallHooks(f.ctx, hooks), []tool.Call{{
		Tool:      candidate,
		Name:      candidate.ID(),
		Arguments: json.RawMessage(`{}`),
	}})
	assert.Equal(t, tool.OutcomeExecuted, report.Results[0].Outcome)
	assert.Equal(t, 1, candidate.projections)
	require.Len(t, recorder.payloads, 1)
	assert.Equal(t, "apply_patch", recorder.payloads[0]["tool_name"])
}

func TestInvoke_EmptyProjectionKeepsNativeIdentity(t *testing.T) {
	for _, matcher := range []string{"*", "apply_patch"} {
		t.Run(matcher, func(t *testing.T) {
			f := newHookFixture(t)
			f.settings(t, "settings.json", hookSettings("PreToolUse", matcher, commandHandler("guard")))
			recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome {
				return Outcome{ExitCode: 2, Stderr: []byte("native guard")}
			}}
			hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
			require.NoError(t, err)
			candidate := &emptyHookProjector{hookTool: &hookTool{id: "apply_patch"}}
			report := tool.Schedule(tool.WithCallHooks(f.ctx, hooks), []tool.Call{{
				Tool:      candidate,
				Name:      candidate.ID(),
				Arguments: json.RawMessage(`{"patch":"--- a/file\n+++ b/file"}`),
			}})
			require.Len(t, report.Results, 1)
			assert.Equal(t, tool.OutcomeFailed, report.Results[0].Outcome)
			assert.Equal(t, 1, candidate.projections)
			require.Len(t, recorder.payloads, 1)
			assert.Equal(t, "apply_patch", recorder.payloads[0]["tool_name"])
		})
	}
}
