package hooks

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type hookDecisionCase struct {
	name          string
	fields        map[string]any
	outcome       Outcome
	refusal       string
	feedback      []string
	warning       string
	ignoredFields []string
	noHooks       bool
}

func TestHookDecisionContracts(t *testing.T) {
	longReason, err := json.Marshal(map[string]any{"decision": "block", "reason": strings.Repeat("界", 10001)})
	require.NoError(t, err)
	tests := append(preToolDecisionContracts(), ignoredHookDecisionContracts()...)
	tests = append(tests, hookDecisionCase{
		name:    "reason is capped in Unicode characters",
		outcome: Outcome{Stdout: longReason},
		refusal: strings.Repeat("界", 10000),
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { checkHookDecisionContract(t, tt) })
	}
}

func preToolDecisionContracts() []hookDecisionCase {
	return []hookDecisionCase{
		{
			name: "exit two beats allow JSON",
			outcome: Outcome{
				ExitCode: 2,
				Stderr:   []byte("guard refused"),
				Stdout:   []byte(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`),
			},
			refusal: "guard refused",
		},
		{
			name: "exit seven uses deny JSON",
			outcome: Outcome{
				ExitCode: 7,
				Stdout: []byte(
					`{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"JSON guard refused"}}`,
				),
			},
			refusal: "JSON guard refused",
		},
		{
			name:    "invalid JSON uses successful exit code",
			fields:  map[string]any{"onFailure": "block"},
			outcome: Outcome{Stdout: []byte(`{"decision":`)},
		},
		{
			name:     "additional context reaches scheduler result",
			outcome:  Outcome{Stdout: []byte(`{"hookSpecificOutput":{"additionalContext":"operator advice"}}`)},
			feedback: []string{"PreToolUse:Bash hook context: operator advice"},
		},
	}
}

func ignoredHookDecisionContracts() []hookDecisionCase {
	return []hookDecisionCase{
		{
			name:   "async ignores denial and exit two",
			fields: map[string]any{"async": true},
			outcome: Outcome{
				ExitCode: 2,
				Stderr:   []byte("ignored"),
				Stdout:   []byte(`{"decision":"block","reason":"ignored"}`),
			},
		},
		{
			name:   "asyncRewake ignores denial and exit two",
			fields: map[string]any{"asyncRewake": true},
			outcome: Outcome{
				ExitCode: 2,
				Stderr:   []byte("ignored"),
				Stdout:   []byte(`{"decision":"block","reason":"ignored"}`),
			},
		},
		{
			name:    "powershell is skipped with warning",
			fields:  map[string]any{"shell": "powershell"},
			warning: "hooks_handler_unsupported",
			noHooks: true,
		},
		{
			name: "updated input and continue false do not erase denial",
			outcome: Outcome{
				Stdout: []byte(
					`{"continue":false,"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"still denied","updatedInput":{"command":"replaced"}}}`,
				),
			},
			refusal:       "still denied",
			ignoredFields: []string{"updatedInput", "continue"},
		},
	}
}

func checkHookDecisionContract(t *testing.T, tt hookDecisionCase) {
	t.Helper()
	f := newHookFixture(t)
	handler := commandHandler("guard")
	maps.Copy(handler, tt.fields)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", handler))
	recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome { return tt.outcome }}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	if tt.noHooks {
		assert.Nil(t, hooks)
		assert.Empty(t, recorder.payloads)
	} else {
		require.NotNil(t, hooks)
		report := tool.Schedule(tool.WithCallHooks(f.ctx, hooks), []tool.Call{bashCall()})
		require.Len(t, report.Results, 1)
		result := report.Results[0]
		require.NotNil(t, result.Result)
		if tt.refusal != "" {
			assert.Equal(t, tool.OutcomeFailed, result.Outcome)
			assert.Equal(t, "PreToolUse:Bash hook error: [guard]: "+tt.refusal, result.Result.Output)
		} else {
			assert.Equal(t, tool.OutcomeExecuted, result.Outcome)
			assert.Equal(t, "executed", result.Result.Output)
		}
		assert.Equal(t, tt.feedback, result.Result.HookFeedback)
	}
	if tt.warning != "" {
		assert.Equal(t, 1, f.logs.FilterMessage(tt.warning).Len())
	}
	var fields []string
	for _, entry := range f.logs.FilterMessage("hooks_output_ignored").All() {
		field, ok := entry.ContextMap()["field"].(string)
		require.True(t, ok)
		fields = append(fields, field)
	}
	assert.ElementsMatch(t, tt.ignoredFields, fields)
}
