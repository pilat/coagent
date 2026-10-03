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
)

// The description states the actual scheduling contract: parallel-safe tools
// share stages, everything else is a barrier, and any failure stops later
// stages. Native multiple tool calls remain the preferred model-facing form.
const batchDescription = `Runs several tool calls from one fallback envelope. Prefer native multiple tool calls for independent work; use batch only as a fallback.

Parameters format:
{
  "calls": [
    {"tool": "read", "params": {"file_path": "src/main.go"}},
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
	calls := make([]tool.Call, len(p.Calls))
	for i, call := range p.Calls {
		calls[i] = tool.Call{Tool: t.registry.Get(call.Tool), Name: call.Tool, Arguments: call.Params}
	}

	report := tool.Schedule(ctx, calls)
	// A batch cannot transfer a nested call's ownership to its outer call ID.
	for i := range report.Results {
		if report.Results[i].Outcome == tool.OutcomeSuspended {
			report.Results[i].Outcome = tool.OutcomeFailed
			report.Summary.Suspended--
			report.Summary.Failed++
		}
	}

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
// calls fabricate neither. Any nested external provenance marks the combined
// result external: after truncation the outer formatter cannot protect only one
// segment of the rendered payload.
func (t *BatchTool) formatResult(calls []BatchCall, report tool.Report) *tool.Result {
	var output strings.Builder

	direct := make([]string, 0)

	successCount := 0

	errorCount := 0

	var images []llmwire.ImageRef

	untrusted := false

	for _, r := range report.Results {
		call := calls[r.Index]

		fmt.Fprintf(&output, "=== %s (call %d) ===\n", call.Tool, r.Index+1)

		switch r.Outcome {
		case tool.OutcomeExecuted, tool.OutcomeFailed:
			if r.Result != nil {
				result := r.Result

				if result.Title != "" {
					fmt.Fprintf(&output, "[%s]\n", result.Title)
				}

				output.WriteString(result.Output)
				output.WriteString("\n")

				untrusted = untrusted || result.Untrusted

				// Typed failures keep the attachments their real result carried
				// (same contract as the native path); skipped/cancelled children
				// fabricate none because they never produce a result at all.
				images = append(images, result.Images...)

				direct = append(direct, result.DirectMessages...)

				if r.Outcome == tool.OutcomeExecuted {
					successCount++
				} else {
					errorCount++
				}

				break
			}

			untrusted = untrusted || tool.IsUntrustedOutputSource(call.Tool)

			fmt.Fprintf(&output, "Error: %v\n", r.Err)

			errorCount++
		case tool.OutcomeSkipped, tool.OutcomeCancelled:
			fmt.Fprintf(&output, "Error: %v\n", r.Err)

			errorCount++
		case tool.OutcomeSuspended:
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
		Title:     fmt.Sprintf("Batch: %d/%d succeeded", successCount, len(calls)),
		Output:    strings.TrimSpace(output.String()),
		IsError:   errorCount > 0,
		Untrusted: untrusted,
		Metadata: map[string]any{
			metaKeyTotal: len(calls),
			"success":    successCount,
			"errors":     errorCount,
		},
		Images:         images,
		DirectMessages: direct,
	}
}
