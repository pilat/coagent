package builtin

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

func TestBatchToolPreservesFormattingContract(t *testing.T) {
	outcomes := []tool.Outcome{
		tool.OutcomeExecuted, tool.OutcomeFailed, tool.OutcomeSkipped,
		tool.OutcomeCancelled, tool.OutcomeSuspended, tool.Outcome(255),
	}
	for _, outcome := range outcomes {
		for _, withResult := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/result=%t", outcome, withResult), func(t *testing.T) {
				candidate := &untrustedScriptedTool{
					scriptedTool: &scriptedTool{
						id: "sample",
					},
				}
				var nested *tool.Result
				if withResult {
					nested = batchFormattingResult()
				}

				calls := []tool.Call{{
					Tool: candidate,
					Name: "sample",
				}}
				report := tool.Report{
					Results: []tool.CallResult{{
						Index:   0,
						Outcome: outcome,
						Result:  nested,
						Err:     errors.New("boom"),
					}},
				}
				actual := NewBatchTool(tool.NewRegistry()).formatResult(calls, report)
				assertBatchFormattingContract(t, actual, nested, outcome)
			})
		}
	}
}

func batchFormattingResult() *tool.Result {
	return &tool.Result{
		Title:  "title",
		Output: "payload\n",
		Images: []llmwire.ImageRef{{
			Path: "/tmp/frame.png",
			Mime: "image/png",
		}},
		DirectMessages: []string{"direct"},
		HookFeedback:   []string{"hook feedback"},
		Untrusted:      true,
		IsError:        true,
	}
}

func assertBatchFormattingContract(t *testing.T, actual, nested *tool.Result, outcome tool.Outcome) {
	t.Helper()

	wantOutput := "=== sample (call 1) ===\nError: boom"
	wantSuccess := 0
	keepsResult := false

	switch outcome {
	case tool.OutcomeExecuted, tool.OutcomeFailed:
		if nested != nil {
			keepsResult = true
			wantOutput = "=== sample (call 1) ===\n[title]\npayload"
			assert.Equal(t, nested.Images, actual.Images)
			assert.Equal(t, nested.DirectMessages, actual.DirectMessages)
			assert.Equal(t, nested.HookFeedback, actual.HookFeedback)
			if outcome == tool.OutcomeExecuted {
				wantSuccess = 1
			}
		}

		assert.True(t, actual.Untrusted)
	case tool.OutcomeSkipped, tool.OutcomeCancelled, tool.OutcomeSuspended:
		assert.False(t, actual.Untrusted)
	default:
		wantOutput = "=== sample (call 1) ===\nError: unexpected outcome 255"
		assert.False(t, actual.Untrusted)
	}

	if !keepsResult {
		assert.Empty(t, actual.Images)
		assert.Empty(t, actual.DirectMessages)
		assert.Empty(t, actual.HookFeedback)
	}

	assert.Equal(t, wantOutput, actual.Output)
	assert.Equal(t, fmt.Sprintf("Batch: %d/1 succeeded", wantSuccess), actual.Title)
	assert.Equal(t, wantSuccess == 0, actual.IsError)
	assert.Equal(t, map[string]any{
		metaKeyTotal: 1,
		"success":    wantSuccess,
		"errors":     1 - wantSuccess,
	}, actual.Metadata)
}
