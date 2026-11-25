package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const block = "block"

func interpret(ctx context.Context, event string, entry handler, out Outcome, err error) decision {
	if err != nil {
		return failureDecision(ctx, event, entry, out, err)
	}

	if isMissingStopScript(event, out) {
		return failureDecision(ctx, event, entry, out, nil)
	}

	stdout := strings.TrimSpace(string(out.Stdout))
	var output hookOutput

	if !strings.HasPrefix(stdout, "{") || !strings.HasSuffix(stdout, "}") {
		return nonJSONDecision(ctx, event, entry, out)
	}

	if err := json.Unmarshal(out.Stdout, &output); err != nil {
		if out.ExitCode == 2 {
			return nonJSONDecision(ctx, event, entry, out)
		}

		return failureDecision(ctx, event, entry, out, errors.New("invalid JSON output"))
	}

	logIgnored(ctx, event, entry.command, output)

	if event == preToolUse && entry.onFailure == block && out.ExitCode != 0 && out.ExitCode != 2 {
		return failureDecision(ctx, event, entry, out, nil)
	}

	return outputDecision(event, entry, out, output)
}

func nonJSONDecision(ctx context.Context, event string, entry handler, out Outcome) decision {
	if out.ExitCode != 0 && out.ExitCode != 2 {
		return failureDecision(ctx, event, entry, out, nil)
	}

	return outputDecision(event, entry, out, hookOutput{})
}

func outputDecision(event string, entry handler, out Outcome, output hookOutput) decision {
	result := decision{
		command: entry.command,
		block:   output.Decision == block,
		reason:  capText(output.Reason),
		context: capText(output.HookSpecificOutput.AdditionalContext),
	}
	if event == preToolUse {
		applyPermission(&result, output)
	}

	if out.ExitCode == 2 {
		result.block = true
		if result.reason == "" {
			result.reason = capText(strings.TrimSpace(string(out.Stderr)))
		}
	}

	if event == stop && output.Continue != nil && !*output.Continue {
		result.block = false
		result.context = ""
	}

	return result
}

func isMissingStopScript(event string, out Outcome) bool {
	if event != stop || out.ExitCode != 2 {
		return false
	}

	if strings.TrimSpace(string(out.Stdout)) != "" {
		return false
	}

	stderr := strings.TrimSpace(string(out.Stderr))
	if strings.ContainsAny(stderr, "\r\n") {
		return false
	}

	return strings.Contains(stderr, "No such file or directory")
}

func failureDecision(ctx context.Context, event string, entry handler, out Outcome, err error) decision {
	cause := fmt.Sprintf("exit code %d", out.ExitCode)
	if err != nil {
		cause = err.Error()
	}

	stderr := string(out.Stderr)
	logger.Ctx(ctx).Warn("hook_failed", zap.String("event", event), zap.String("command", entry.command),
		zap.String("cause", cause), zap.String("stderr", stderr[max(0, len(stderr)-512):]))

	return decision{
		command: entry.command,
		block:   event == preToolUse && entry.onFailure == block,
		reason:  capText("hook failed: " + cause),
	}
}

func applyPermission(result *decision, output hookOutput) {
	permission := output.HookSpecificOutput.PermissionDecision
	if permission == "deny" || permission == "ask" || permission == "defer" {
		result.block = true
		result.reason = capText(output.HookSpecificOutput.PermissionDecisionReason)
	}

	if output.Decision == "deny" {
		result.block = true
	}
}

func logIgnored(ctx context.Context, event, command string, output hookOutput) {
	specific := output.HookSpecificOutput
	logIgnoredUpdate(ctx, event, command, "updatedInput", output.UpdatedInput, specific.UpdatedInput)
	logIgnoredUpdate(ctx, event, command, "updatedToolOutput", output.UpdatedToolOutput, specific.UpdatedToolOutput)
	logIgnoredUpdate(
		ctx,
		event,
		command,
		"updatedMCPToolOutput",
		output.UpdatedMCPToolOutput,
		specific.UpdatedMCPToolOutput,
	)

	if event != stop && output.Continue != nil && !*output.Continue {
		logger.Ctx(ctx).Warn("hooks_output_ignored", zap.String("event", event), zap.String("field", "continue"))
	}

	logHookMessage(ctx, event, "systemMessage", output.SystemMessage)
	logHookMessage(ctx, event, "stopReason", output.StopReason)
}

func logIgnoredUpdate(ctx context.Context, event, command, key string, top, nested json.RawMessage) {
	if top == nil && nested == nil {
		return
	}

	logger.Ctx(ctx).Warn("hooks_output_ignored",
		zap.String("event", event), zap.String("command", command), zap.String("field", key))
}

func logHookMessage(ctx context.Context, event, key, value string) {
	if value == "" {
		return
	}

	logger.Ctx(ctx).Info("hooks_message",
		zap.String("event", event), zap.String("field", key), zap.String("message", capText(value)))
}

func capText(text string) string {
	runes := []rune(text)
	return string(runes[:min(len(runes), 10000)])
}
