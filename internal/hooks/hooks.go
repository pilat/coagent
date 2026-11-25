package hooks

import (
	"context"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const (
	preToolUse  = "PreToolUse"
	postToolUse = "PostToolUse"
	stop        = "Stop"
)

var _ tool.CallHooks = (*Hooks)(nil)

type Hooks struct {
	workDir   string
	sessionID int64
	launch    Launcher
	handlers  map[string][]handler
}

func Load(ctx context.Context, workDir string, sessionID int64, launch Launcher) (*Hooks, error) {
	handlers, err := loadSettings(ctx, workDir)
	if err != nil {
		return nil, err
	}
	var hooks *Hooks

	if hasHandlers(handlers) {
		hooks = &Hooks{
			workDir:   workDir,
			sessionID: sessionID,
			launch:    launch,
			handlers:  handlers,
		}
	}

	return hooks, nil
}

func (h *Hooks) BeforeCall(ctx context.Context, call tool.Call) tool.HookVerdict {
	calls, names := hookCalls(call)
	var verdict tool.HookVerdict

	for _, visible := range calls {
		current := h.beforeProjected(ctx, call, visible, matchNames(visible, names))
		if verdict.Refusal == "" {
			verdict.Refusal = current.Refusal
		}

		verdict.Context = append(verdict.Context, current.Context...)
	}

	if ctx.Err() != nil {
		verdict.Refusal = "PreToolUse hook interrupted: the session is stopping"
	}

	return verdict
}

func (h *Hooks) AfterCall(ctx context.Context, call tool.Call, result *tool.Result) []string {
	calls, names := hookCalls(call)
	var feedback []string

	for _, visible := range calls {
		payload := postToolPayload{
			toolPayload:  h.toolPayload(ctx, postToolUse, call, visible),
			ToolResponse: defaultToolResponse{Output: result.Output},
		}
		if responder, ok := call.Tool.(tool.HookResponder); ok {
			payload.ToolResponse = responder.HookResponse(result)
		}

		for _, decision := range h.run(ctx, postToolUse, matchNames(visible, names), payload) {
			prefix := postToolUse + ":" + visible.Name.Name + " hook feedback: [" + decision.command + "]: "
			if decision.block {
				feedback = append(feedback, prefix+decision.reason)
			}

			if decision.context != "" {
				feedback = append(feedback, prefix+decision.context)
			}
		}
	}

	return feedback
}

func (h *Hooks) beforeProjected(
	ctx context.Context,
	call tool.Call,
	visible tool.HookCall,
	names []string,
) tool.HookVerdict {
	var refusal []string
	var feedback []string

	for _, result := range h.run(ctx, preToolUse, names, h.toolPayload(ctx, preToolUse, call, visible)) {
		if result.block {
			refusal = append(
				refusal,
				preToolUse+":"+visible.Name.Name+" hook error: ["+result.command+"]: "+result.reason,
			)
		}

		if result.context != "" {
			feedback = append(feedback, preToolUse+":"+visible.Name.Name+" hook context: "+result.context)
		}
	}

	return tool.HookVerdict{
		Refusal: strings.Join(refusal, "\n\n"),
		Context: feedback,
	}
}

func hookIdentity(call tool.Call) tool.HookCall {
	name := tool.HookName{Name: call.Tool.ID()}
	if named, ok := call.Tool.(tool.HookNamed); ok {
		name = named.HookName()
		if name.Name == "" {
			name.Name = call.Tool.ID()
		}
	}

	visible := tool.HookCall{
		ToolID: call.Tool.ID(),
		Name:   name,
		Input:  call.Arguments,
	}

	return visible
}

func hookCalls(call tool.Call) ([]tool.HookCall, []string) {
	visible := hookIdentity(call)
	if len(call.Projection) > 0 {
		nativeNames := matchNames(visible, nil)
		return call.Projection, nativeNames
	}

	return []tool.HookCall{visible}, nil
}

func matchNames(call tool.HookCall, native []string) []string {
	names := append([]string{call.ToolID, call.Name.Name}, call.Name.Aliases...)
	return append(names, native...)
}
