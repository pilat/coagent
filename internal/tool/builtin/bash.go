package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

const (
	defaultBashTimeout = 2 * time.Minute
	maxBashTimeout     = 10 * time.Minute
	maxOutputSize      = 100 * 1024 // 100KB
	noOutput           = "(no output)"
	bashDescription    = `Executes a given bash command in a shell session with optional timeout.

IMPORTANT: This tool is for terminal operations like git, npm, docker, build commands, etc. DO NOT use it for file operations (reading, writing, editing, searching, finding files) - use the dedicated tools for this instead.

Avoid using Bash with find, grep, cat, head, tail, sed, awk, or echo commands, unless explicitly instructed. Instead, always prefer using the dedicated tools:
- File search: Use Glob (NOT find or ls)
- Content search: Use Grep (NOT grep or rg)
- Read files: Use Read (NOT cat/head/tail)
- Edit files: Use Edit (NOT sed/awk)
- Write files: Use Write (NOT echo >/cat <<EOF)

Command Execution:
- Always quote file paths that contain spaces with double quotes (e.g., rm "path with spaces/file.txt")
- The command argument is required
- You can specify an optional timeout in milliseconds (default: 120000, max: 600000)
- It is very helpful if you write a clear, concise description of what this command does in 5-10 words
- Both stdout and stderr are captured

Parallel vs Sequential Commands:
- If the commands are independent and can run in parallel, make multiple Bash tool calls in a single response
- If the commands depend on each other and must run sequentially, use a single Bash call with '&&' to chain them together
- Use ';' only when you need to run commands sequentially but don't care if earlier commands fail
- DO NOT use newlines to separate commands (newlines are ok in quoted strings)
- AVOID using 'cd <directory> && <command>'. Use the work_dir parameter to change directories instead

Limits:
- Output is truncated at 100KB
- Commands timing out are killed and partial output is returned

Git Safety Protocol:
- NEVER update the git config
- NEVER run destructive/irreversible git commands (like push --force, hard reset, etc.) unless explicitly requested
- NEVER skip hooks (--no-verify, --no-gpg-sign, etc.) unless explicitly requested
- NEVER run force push to main/master, warn the user if they request it
- Avoid git commit --amend. ONLY use --amend when ALL conditions are met:
  (1) User explicitly requested amend, OR commit SUCCEEDED but pre-commit hook auto-modified files that need including
  (2) HEAD commit was created by you in this conversation
  (3) Commit has NOT been pushed to remote
- CRITICAL: If commit FAILED or was REJECTED by hook, NEVER amend - fix the issue and create a NEW commit
- CRITICAL: If you already pushed to remote, NEVER amend unless user explicitly requests it (requires force push)
- NEVER commit changes unless the user explicitly asks you to
- If there are no changes to commit, do not create an empty commit

Pull Requests:
- Use gh command via Bash tool for ALL GitHub-related tasks
- When creating PRs, analyze ALL commits that will be included (not just the latest)
- Return the PR URL when done so the user can see it

Examples:
- "git status"
- "git add . && git commit -m 'message'"
- "npm install"
- "go build ./..."`
)

var _ tool.Tool = (*bashTool)(nil)

// bashParams are the parameters for the bash tool.
type bashParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // milliseconds
	WorkDir string `json:"work_dir,omitempty"`
}

type bashTool struct {
	workDir string
	runner  bashsandbox.Runner
}

func newBashTool(workDir string, runner bashsandbox.Runner) *bashTool {
	return &bashTool{workDir: workDir, runner: runner}
}

func (t *bashTool) ID() string          { return "bash" }
func (t *bashTool) Description() string { return bashDescription }

func (t *bashTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The command to execute"
			},
			"timeout": {
				"type": "integer",
				"description": "Timeout in milliseconds (max 600000, default 120000)"
			},
			"work_dir": {
				"type": "string",
				"description": "Working directory for the command (defaults to tool's working directory)"
			}
		},
		"required": ["command"]
	}`)
}

func (t *bashTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.bash")

	var p bashParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Command == "" {
		log.Warn("empty_command")
		return nil, errors.New("command is required")
	}

	timeout := bashTimeout(p.Timeout)

	workDir := t.workDir
	if p.WorkDir != "" {
		workDir = p.WorkDir
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Debug("executing",
		zap.String("command", p.Command),
		zap.String("workDir", workDir),
		zap.Duration("timeout", timeout),
	)

	return t.run(ctx, log, p.Command, workDir, timeout)
}

func bashTimeout(ms int) time.Duration {
	if ms <= 0 {
		return defaultBashTimeout
	}

	t := time.Duration(ms) * time.Millisecond
	if t > maxBashTimeout {
		return maxBashTimeout
	}

	return t
}

func (t *bashTool) run(
	ctx context.Context,
	log *zap.Logger,
	command, workDir string,
	timeout time.Duration,
) (*tool.Result, error) {
	cmd, err := t.runner.ShellCommand(ctx, command, workDir)
	if err != nil {
		return nil, fmt.Errorf("create bash command: %w", err)
	}

	configureCommandCancellation(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	stdoutLen, stderrLen := stdout.Len(), stderr.Len()
	output, truncated := combineOutput(&stdout, &stderr)

	title := command
	if len(title) > 50 {
		title = title[:50] + "..."
	}

	if err != nil {
		return t.handleErr(ctx, log, command, title, output, timeout, truncated, err)
	}

	if output == "" {
		output = noOutput
	}

	log.Debug("complete",
		zap.String("command", command),
		zap.Int("exitCode", 0),
		zap.Int("stdout", stdoutLen),
		zap.Int("stderr", stderrLen),
		zap.Bool("truncated", truncated))
	log.Info("executed",
		zap.String("command", command),
		zap.Int("exitCode", 0),
		zap.Int("outputSize", len(output)),
	)

	return &tool.Result{
		Title:  title,
		Output: strings.TrimSuffix(output, "\n"),
		Metadata: map[string]any{
			metaKeyExitCode:  0,
			metaKeyTimedOut:  false,
			metaKeyTruncated: truncated,
		},
	}, nil
}

func (t *bashTool) handleErr(
	ctx context.Context,
	log *zap.Logger,
	command, title, output string,
	timeout time.Duration,
	truncated bool,
	err error,
) (*tool.Result, error) {
	if ctx.Err() == context.DeadlineExceeded {
		log.Warn("command_timeout", zap.String("command", command), zap.Duration("timeout", timeout))

		return &tool.Result{
			Title:  title,
			Output: output + "\n\n(Command timed out after " + timeout.String() + ")",
			Metadata: map[string]any{
				metaKeyExitCode:  -1,
				metaKeyTimedOut:  true,
				metaKeyTruncated: truncated,
			},
		}, nil
	}

	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		exitCode := exitErr.ExitCode()
		log.Debug("command_failed", zap.String("command", command), zap.Int("exitCode", exitCode))

		if output == "" {
			output = noOutput
		}

		if hint := sandboxHint(output, t.runner.WritableRoots()); hint != "" {
			output += "\n\n" + hint
		}

		log.Info(
			"executed",
			zap.String("command", command),
			zap.Int("exitCode", exitCode),
			zap.Int("outputSize", len(output)),
		)

		return &tool.Result{
			Title:  title,
			Output: strings.TrimSuffix(output, "\n"),
			Metadata: map[string]any{
				metaKeyExitCode:  exitCode,
				metaKeyTimedOut:  false,
				metaKeyTruncated: truncated,
			},
		}, nil
	}

	log.Warn("execution_error", zap.String("command", command), zap.Error(err))

	return nil, fmt.Errorf("execute command: %w", err)
}

func combineOutput(stdout, stderr *bytes.Buffer) (string, bool) {
	var out strings.Builder
	if stdout.Len() > 0 {
		out.Write(stdout.Bytes())
	}

	if stderr.Len() > 0 {
		if out.Len() > 0 {
			out.WriteString("\n")
		}

		out.WriteString("[stderr]\n")
		out.Write(stderr.Bytes())
	}

	result := out.String()
	if len(result) > maxOutputSize {
		return result[:maxOutputSize] + "\n\n(Output truncated)", true
	}

	return result, false
}
