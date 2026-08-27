package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	batchDescription = `Executes multiple independent tool calls in parallel to reduce latency.

USING THE BATCH TOOL WILL IMPROVE PERFORMANCE.

Parameters format:
{
  "calls": [
    {"tool": "read", "params": {"path": "src/main.go"}},
    {"tool": "grep", "params": {"pattern": "func main", "path": "src"}},
    {"tool": "bash", "params": {"command": "git status", "description": "Check git status"}}
  ]
}

Limits:
- 1-25 tool calls per batch
- All calls start in parallel; ordering NOT guaranteed
- Partial failures do not stop other calls
- Nested batch is NOT allowed

Good use cases:
- Read multiple files at once
- grep + glob + read combos
- Multiple independent bash commands
- Multi-part edits on same or different files

When NOT to use:
- Operations that depend on prior tool output (e.g., create then read same file)
- Ordered stateful mutations where sequence matters

Batching tool calls yields 2-5x efficiency gain.`
)

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

	callResult struct {
		index  int
		tool   string
		result *tool.Result
		err    error
	}
)

var _ tool.RegistryBound = (*BatchTool)(nil)

func NewBatchTool(registry tool.Registry) *BatchTool {
	return &BatchTool{registry: registry}
}

func (t *BatchTool) ID() string          { return tool.IDBatch }
func (t *BatchTool) Description() string { return batchDescription }

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
				"description": "List of tool calls to execute in parallel",
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

	results := t.runParallel(ctx, p.Calls)

	return t.formatResult(results, len(p.Calls)), nil
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
		if call.Tool == tool.IDBatch {
			return fmt.Errorf("call %d: nested batch is not allowed", i+1)
		}

		if call.Tool == tool.IDSkill {
			return fmt.Errorf("call %d: skill must be invoked directly", i+1)
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

func (t *BatchTool) runParallel(ctx context.Context, calls []BatchCall) []callResult {
	results := make([]callResult, len(calls))

	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)

		go func(idx int, c BatchCall) {
			defer wg.Done()

			impl := t.registry.Get(c.Tool)
			result, err := impl.Execute(ctx, c.Params)
			results[idx] = callResult{
				index:  idx,
				tool:   c.Tool,
				result: result,
				err:    err,
			}
		}(i, call)
	}

	wg.Wait()

	return results
}

func (t *BatchTool) formatResult(results []callResult, total int) *tool.Result {
	var output strings.Builder

	successCount := 0
	errorCount := 0
	var images []llmwire.ImageRef

	for _, r := range results {
		fmt.Fprintf(&output, "=== %s (call %d) ===\n", r.tool, r.index+1)

		if r.err != nil {
			fmt.Fprintf(&output, "Error: %v\n", r.err)

			errorCount++
		} else {
			if r.result.Title != "" {
				fmt.Fprintf(&output, "[%s]\n", r.result.Title)
			}

			output.WriteString(r.result.Output)
			output.WriteString("\n")

			// Failed children produce no refs; successful ones keep their pixel
			// attachments in nested call order.
			images = append(images, r.result.Images...)

			successCount++
		}

		output.WriteString("\n")
	}

	return &tool.Result{
		Title:  fmt.Sprintf("Batch: %d/%d succeeded", successCount, total),
		Output: strings.TrimSpace(output.String()),
		Metadata: map[string]any{
			"total":   total,
			"success": successCount,
			"errors":  errorCount,
		},
		Images: images,
	}
}
