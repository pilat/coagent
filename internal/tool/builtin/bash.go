package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

const (
	// foregroundGrace is how long a command runs inline before promotion.
	foregroundGrace = 10 * time.Second

	// foregroundJoinMargin covers terminalization commit after a
	// sub-grace deadline expires in the foreground.
	foregroundJoinMargin = 2 * time.Second

	// defaultProcessDeadline is the model-facing absolute process deadline.
	defaultProcessDeadline = 10 * time.Minute

	// maxProcessDeadline is the thirty-minute hard cap.
	maxProcessDeadline = 30 * time.Minute

	maxOutputSize = 100 * 1024 // 100KB inline cap
	noOutput      = "(no output)"

	metaKeyProcessID = "process_id"

	bashDescription = `Executes a given bash command in a shell session with optional timeout.

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
- Both stdout and stderr are captured into one combined stream

Parallel vs Sequential Commands:
- If the commands are independent and can run in parallel, make multiple Bash tool calls in a single response
- If the commands depend on each other and must run sequentially, use a single Bash call with '&&' to chain them together
- Use ';' only when you need to run commands sequentially but don't care if earlier commands fail
- DO NOT use newlines to separate commands (newlines are ok in quoted strings)
- AVOID using 'cd <directory> && <command>'. Use the work_dir parameter to change directories instead

Limits:
- Output is truncated at 100KB inline; larger output stays in the reported output file
- Foreground failures (nonzero exit, timeout, output overflow) are reported as errors

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
- "npm install"
- "go build ./..."`

	backgroundDescriptionSuffix = `

Background Execution:
- Long-running commands (builds, test suites, servers under 30 minutes) run automatically in the background after 10 seconds
- Set "background": true to background immediately without the 10-second wait
- A backgrounded command returns a process ID and an absolute output path; completion is delivered to you automatically and wakes this session - do NOT poll
- If you need current output while working independently, read a small suffix of the output file with the tail tool
- "timeout" is the absolute process deadline in milliseconds (default 600000, max 1800000); a deadline below 10000 expires in the foreground
- Only finite commands are supported; a command reaching its deadline is killed as a complete process group`
)

var _ tool.Tool = (*bashTool)(nil)

// bashParams are the parameters for the bash tool.
type bashParams struct {
	Command    string `json:"command"`
	Timeout    int    `json:"timeout,omitempty"`    // milliseconds
	WorkDir    string `json:"work_dir,omitempty"`   //
	Background bool   `json:"background,omitempty"` //
}

type bashTool struct {
	workDir string
	runner  bashsandbox.Runner
	process *backgroundprocess.Service
	session int64
	root    int64
}

func newBashTool(
	workDir string,
	runner bashsandbox.Runner,
	process *backgroundprocess.Service,
	sessionID, rootID int64,
) *bashTool {
	return &bashTool{
		workDir: workDir,
		runner:  runner,
		process: process,
		session: sessionID,
		root:    rootID,
	}
}

func (t *bashTool) ID() string          { return "bash" }
func (t *bashTool) ParallelSafe() bool  { return false }
func (t *bashTool) Description() string { return bashDescription + backgroundDescriptionSuffix }

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
				"description": "Absolute process deadline in milliseconds (default 600000, max 1800000; below 10000 expires in the foreground)"
			},
			"work_dir": {
				"type": "string",
				"description": "Working directory for the command (defaults to tool's working directory)"
			},
			"background": {
				"type": "boolean",
				"description": "Run immediately as a background process and return its ID and output path"
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

	deadline := processDeadline(p.Timeout)
	workDir := t.workDir

	if p.WorkDir != "" {
		workDir = p.WorkDir
	}

	log.Debug("executing",
		zap.String("workDir", workDir),
		zap.Duration("deadline", deadline),
		zap.Bool("background", p.Background),
	)

	return t.run(ctx, p, workDir, deadline)
}

// processDeadline resolves the model-provided timeout: omitted/nonpositive
// selects the default; values above the cap clamp to it.
func processDeadline(ms int) time.Duration {
	if ms <= 0 {
		return defaultProcessDeadline
	}

	d := time.Duration(ms) * time.Millisecond
	if d > maxProcessDeadline {
		return maxProcessDeadline
	}

	return d
}

func (t *bashTool) run(
	ctx context.Context,
	p bashParams,
	workDir string,
	deadline time.Duration,
) (*tool.Result, error) {
	spec := backgroundprocess.Spec{
		SessionID:     t.session,
		RootSessionID: t.root,
		ToolCallID:    tool.CallIDFromContext(ctx),
		Deadline:      deadline,
		Advertise:     p.Background,
	}

	start := time.Now()

	record, err := t.process.Start(ctx, spec, func(processCtx context.Context) (*exec.Cmd, error) {
		// The command-construction authority stays with the sandbox runner;
		// only the lifetime context is owned by the process service.
		cmd, cmdErr := t.runner.ShellCommand(processCtx, p.Command, workDir)
		if cmdErr != nil {
			return nil, fmt.Errorf("create bash command: %w", cmdErr)
		}

		return cmd, nil
	})
	if err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	// Foreground wait: a deadline shorter than the grace can only expire in
	// the foreground, with a small margin for terminalization to commit; a
	// normal command waits exactly the grace and prefers an available result
	// at the boundary before advertising.
	wait := deadline
	if wait < foregroundGrace {
		wait += foregroundJoinMargin
	} else {
		wait = foregroundGrace
	}

	if !p.Background {
		if result, ok := t.waitForeground(ctx, record, start.Add(wait)); ok {
			return result, nil
		}
	}

	return t.backgroundedResult(record), nil
}

// waitForeground polls the ledger until the grace expires; the completion
// fact for the foreground path is produced here, not through the wake event.
func (t *bashTool) waitForeground(
	ctx context.Context,
	record backgroundprocess.Process,
	until time.Time,
) (*tool.Result, bool) {
	for time.Now().Before(until) {
		current, err := t.process.Store().GetProcess(ctx, record.ID)
		if err != nil {
			return nil, false
		}

		if current.State.Terminal() {
			return t.foregroundResult(ctx, current), true
		}

		time.Sleep(25 * time.Millisecond)
	}

	current, err := t.process.Store().GetProcess(ctx, record.ID)
	if err == nil && current.State.Terminal() {
		return t.foregroundResult(ctx, current), true
	}

	return nil, false
}

func (t *bashTool) foregroundResult(ctx context.Context, record backgroundprocess.Process) *tool.Result {
	title := t.title(record)

	// Only terminal states reach this; the default covers the cancelled,
	// interrupted, and drain-timeout outcomes. Running cannot be observed here
	// because waitForeground gates on Terminal().
	//nolint:exhaustive // Terminal states only; see comment above.
	switch record.State {
	case backgroundprocess.StateCompleted:
		output := t.inlineOutput(ctx, record, false)

		return &tool.Result{
			Title:  title,
			Output: output,
			Metadata: map[string]any{
				metaKeyProcessID: record.ID,
				metaKeyExitCode:  *record.ExitCode,
				metaKeyTimedOut:  false,
				metaKeyTruncated: record.OutputSize > maxOutputSize,
			},
		}
	case backgroundprocess.StateFailed:
		output := t.inlineOutput(ctx, record, true)
		if hint := t.failureHint(output); hint != "" {
			output += "\n\n" + hint
		}

		return &tool.Result{
			Title:   title,
			Output:  output,
			IsError: true,
			Metadata: map[string]any{
				metaKeyExitCode:  *record.ExitCode,
				metaKeyTimedOut:  false,
				metaKeyTruncated: record.OutputSize > maxOutputSize,
			},
		}
	case backgroundprocess.StateTimedOut:
		output := t.inlineOutput(ctx, record, true) +
			"\n\n(Command timed out after " + record.Deadline.Sub(record.CreatedAt).String() + ")"

		return &tool.Result{
			Title:   title,
			Output:  output,
			IsError: true,
			Metadata: map[string]any{
				metaKeyExitCode:  -1,
				metaKeyTimedOut:  true,
				metaKeyTruncated: false,
			},
		}
	case backgroundprocess.StateOutputLimitExceeded:
		return &tool.Result{
			Title: title,
			Output: "Output exceeded the 100 MiB capture cap; the process group was killed." +
				"\nFull output: " + record.OutputPath,
			IsError: true,
			Metadata: map[string]any{
				metaKeyProcessID: record.ID,
			},
		}
	default:
		output := t.inlineOutput(ctx, record, true)

		return &tool.Result{
			Title:   title,
			Output:  output + "\n\n(command terminated: " + string(record.State) + ")",
			IsError: true,
			Metadata: map[string]any{
				metaKeyExitCode:  -1,
				metaKeyTimedOut:  false,
				metaKeyTruncated: false,
			},
		}
	}
}

// inlineOutput renders a bounded preview from the output file. keepFile
// preserves the file for a failed result's path reference; a successful
// inline result removes the unadvertised candidate file.
func (t *bashTool) inlineOutput(ctx context.Context, record backgroundprocess.Process, keepFile bool) string {
	if text, ok := backgroundprocess.ExtractTail(record.OutputPath, 100000, maxOutputSize); ok {
		if strings.TrimSpace(text) == "" {
			return noOutput
		}

		return strings.TrimSuffix(text, "\n")
	}

	if keepFile {
		return "(output unavailable)"
	}

	_ = t.process.RemoveOutput(ctx, record.ID)

	return "Output exceeded the inline limit (" + fmt.Sprintf("%.1f", float64(record.OutputSize)/(100*1024)) +
		" KB); read a suffix of " + record.OutputPath + " with the tail tool"
}

func (t *bashTool) title(record backgroundprocess.Process) string {
	_ = record

	return "command"
}

// failureHint appends the sandbox self-diagnosis hint to a failed foreground
// result when the observed output looks like a sandbox write denial.
func (t *bashTool) failureHint(output string) string {
	if hint := sandboxHint(output, t.runner.WritableRoots(), t.runner.ReadScope(), t.workDir); hint != "" {
		return hint
	}

	return ""
}

// backgroundedResult is the immediate response for an advertised process.
func (t *bashTool) backgroundedResult(record backgroundprocess.Process) *tool.Result {
	return &tool.Result{
		Title: "background process started",
		Output: "Process " + record.ID + " runs in the background." +
			"\nOutput file: " + record.OutputPath +
			"\nCompletion will be delivered automatically and will wake this session; do not poll." +
			"\nUse the tail tool on the output file if you need current output for independent work.",
		Metadata: map[string]any{
			metaKeyProcessID: record.ID,
		},
	}
}
