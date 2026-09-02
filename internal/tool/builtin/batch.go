package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/toolexec"
)

// The description states the actual scheduling contract: parallel-safe tools
// share stages, everything else is a barrier, and any failure stops later
// stages. Native multiple tool calls remain the preferred model-facing form.
const batchDescription = `Runs several tool calls from one fallback envelope. Prefer native multiple tool calls for independent work; use batch only as a fallback.

Parameters format:
{
  "calls": [
    {"tool": "read", "params": {"path": "src/main.go"}},
    {"tool": "grep", "params": {"pattern": "func main", "path": "src"}}
  ]
}

Limits and scheduling:
- 1-25 tool calls per batch
- Calls run in order: parallel-safe tools in a run share up to four concurrent slots; every other tool runs alone as a barrier
- A failed, skipped, or cancelled call stops every later stage; earlier results are kept
- Nested batch is NOT allowed; skill, activation-only, suspending, and unknown tools must be invoked directly`

type (
	BatchParams struct {
		Calls []BatchCall `json:"calls"`
	}

	BatchCall struct {
		Tool   string          `json:"tool"`
		Params json.RawMessage `json:"params"`
	}

	BatchTool struct {
		registry tool.Registry
	}

	// nestedToolCall pairs one batch entry with the registry instance resolved
	// for it, so classification and execution cannot observe different registry
	// states.
	nestedToolCall struct {
		call BatchCall
		tool tool.Tool
	}

	// nestedResult carries one nested call's raw result; it is nil for calls
	// that failed without producing a typed payload.
	nestedResult struct {
		result *tool.Result
	}
)

var _ tool.RegistryBound = (*BatchTool)(nil)

func NewBatchTool(registry tool.Registry) *BatchTool {
	return &BatchTool{registry: registry}
}

func (t *BatchTool) ID() string          { return tool.IDBatch }
func (t *BatchTool) Description() string { return batchDescription }

// ParallelSafe is always false: nested calls are scheduled internally by the
// common executor.
func (t *BatchTool) ParallelSafe() bool { return false }

// BindRegistry re-targets batch at the registry it is being served from, so a
// filtered tool set stays the only thing a batch can reach.
func (t *BatchTool) BindRegistry(reg tool.Registry) tool.Tool {
	return NewBatchTool(reg)
}

func (t *BatchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"calls": {
				"type": "array",
				"description": "List of tool calls to execute; parallel-safe tools share stages, others run alone",
				"items": {
					"type": "object",
					"properties": {
						"tool": {
							"type": "string",
							"description": "The tool ID to call"
						},
						"params": {
							"type": "object",
							"description": "Parameters for the tool"
						}
					},
					"required": ["tool", "params"]
				}
			}
		},
		"required": ["calls"]
	}`)
}

func (t *BatchTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p BatchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if err := t.validateCalls(p.Calls); err != nil {
		return nil, err
	}

	// Resolve each nested tool once: classification and execution see the
	// same registry view the batch was served from.
	calls := make([]toolexec.Call[nestedToolCall], len(p.Calls))
	for i, call := range p.Calls {
		tl := t.registry.Get(call.Tool)

		calls[i] = toolexec.Call[nestedToolCall]{
			Call: nestedToolCall{
				call: call,
				tool: tl,
			},
			ParallelSafe: tl != nil && tl.ParallelSafe(),
		}
	}

	exec := func(callCtx context.Context, _ int, nc nestedToolCall) toolexec.Invocation[nestedResult] {
		// Validation rejects unknown tools before planning; the nil guard
		// keeps the executor contract honest if a registry view changes anyway.
		if nc.tool == nil {
			return nestedFailure(fmt.Errorf("unknown tool %q", nc.call.Tool))
		}

		result, err := nc.tool.Execute(callCtx, nc.call.Params)
		if err != nil {
			// Nested calls never carry pending-external semantics through the
			// outer ID: a suspend attempt is an ordinary nested failure.
			return nestedFailure(fmt.Errorf("execute %s: %w", nc.call.Tool, err))
		}

		if result == nil {
			return nestedFailure(fmt.Errorf("execute %s: tool returned nil result", nc.call.Tool))
		}

		inv := toolexec.Invocation[nestedResult]{
			Outcome: toolexec.OutcomeExecuted,
			Result:  nestedResult{result: result},
		}

		if result.IsError {
			inv.Outcome = toolexec.OutcomeFailed
		}

		return inv
	}

	report := toolexec.Schedule(ctx, calls, exec)

	log := logger.Ctx(ctx).Named("tool.batch")
	log.Info("tool_schedule",
		zap.Int("calls", report.Summary.Calls),
		zap.Int("stages", report.Summary.Stages),
		zap.Int("max_parallel", report.Summary.MaxParallel),
		zap.Int("executed", report.Summary.Executed),
		zap.Int("skipped", report.Summary.Skipped),
		zap.Int("failed", report.Summary.Failed),
		zap.Int("suspended", report.Summary.Suspended),
		zap.Int64("duration_ms", report.Summary.DurationMS),
	)

	return t.formatResult(p.Calls, report), nil
}

func nestedFailure(err error) toolexec.Invocation[nestedResult] {
	return toolexec.Invocation[nestedResult]{
		Outcome: toolexec.OutcomeFailed,
		Err:     err,
	}
}

func (t *BatchTool) validateCalls(calls []BatchCall) error {
	const maxBatchSize = 25

	if len(calls) == 0 {
		return errors.New("at least one call is required")
	}

	if len(calls) > maxBatchSize {
		return fmt.Errorf("maximum %d calls allowed in a batch, got %d", maxBatchSize, len(calls))
	}

	for i, call := range calls {
		if call.Params == nil {
			return fmt.Errorf("call %d: params object is required", i+1)
		}

		if call.Tool == tool.IDBatch {
			return fmt.Errorf("call %d: nested batch is not allowed", i+1)
		}

		if call.Tool == tool.IDSkill {
			return fmt.Errorf("call %d: skill must be invoked directly", i+1)
		}

		if _, activated := t.registry.Get(call.Tool).(tool.ActivationDeclarer); activated {
			return fmt.Errorf("call %d: %s requires a user command turn and must be called directly", i+1, call.Tool)
		}

		// A suspending tool answers after the loop stops; batch cannot carry that
		// through, and would report a result for work still in flight.
		if tool.IsExternalCall(call.Tool) {
			return fmt.Errorf("call %d: %s suspends the session and must be called directly", i+1, call.Tool)
		}

		if t.registry.Get(call.Tool) == nil {
			return fmt.Errorf("call %d: unknown tool %q", i+1, call.Tool)
		}
	}

	return nil
}

// formatResult renders ordered nested outcomes back into one result. A typed
// failure keeps its payload, images and direct messages; skipped and cancelled
// calls fabricate neither.
func (t *BatchTool) formatResult(calls []BatchCall, report toolexec.Report[nestedResult]) *tool.Result {
	var output strings.Builder

	direct := make([]string, 0)

	successCount := 0

	errorCount := 0

	var images []llmwire.ImageRef

	for _, r := range report.Results {
		call := calls[r.Index]

		fmt.Fprintf(&output, "=== %s (call %d) ===\n", call.Tool, r.Index+1)

		switch r.Outcome {
		case toolexec.OutcomeExecuted, toolexec.OutcomeFailed:
			if r.Result.result != nil {
				result := r.Result.result

				if result.Title != "" {
					fmt.Fprintf(&output, "[%s]\n", result.Title)
				}

				output.WriteString(result.Output)
				output.WriteString("\n")

				// Typed failures keep the attachments their real result carried
				// (same contract as the native path); skipped/cancelled children
				// fabricate none because they never produce a result at all.
				images = append(images, result.Images...)

				direct = append(direct, result.DirectMessages...)

				if r.Outcome == toolexec.OutcomeExecuted {
					successCount++
				} else {
					errorCount++
				}

				break
			}

			fmt.Fprintf(&output, "Error: %v\n", r.Err)

			errorCount++
		case toolexec.OutcomeSkipped, toolexec.OutcomeCancelled:
			fmt.Fprintf(&output, "Error: %v\n", r.Err)

			errorCount++
		case toolexec.OutcomeSuspended:
			// Validation rejects suspending tools, so this is unreachable
			// today; if it ever happens the call must not vanish silently.
			fmt.Fprintf(&output, "Error: %v\n", r.Err)

			errorCount++
		default:
			// A contract violation upstream must not silently vanish from the
			// success/error accounting.
			fmt.Fprintf(&output, "Error: unexpected outcome %d\n", uint8(r.Outcome))

			errorCount++
		}

		output.WriteString("\n")
	}

	return &tool.Result{
		Title:   fmt.Sprintf("Batch: %d/%d succeeded", successCount, len(calls)),
		Output:  strings.TrimSpace(output.String()),
		IsError: errorCount > 0,
		Metadata: map[string]any{
			"total":   len(calls),
			"success": successCount,
			"errors":  errorCount,
		},
		Images:         images,
		DirectMessages: direct,
	}
}
