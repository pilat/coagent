package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
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

	// defaultProcessDeadline is the model-facing absolute process deadline.
	defaultProcessDeadline = 10 * time.Minute

	// maxProcessDeadline is the thirty-minute hard cap.
	maxProcessDeadline = 30 * time.Minute

	maxOutputSize = 100 * 1024 // 100KB inline cap
	noOutput      = "(no output)"

	metaKeyProcessID = "process_id"
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
	process backgroundprocess.Service
	project string
	session int64
	root    int64
}

func newBashTool(
	workDir string,
	runner bashsandbox.Runner,
	process backgroundprocess.Service,
	projectDir string,
	sessionID, rootID int64,
) *bashTool {
	return &bashTool{
		workDir: workDir,
		runner:  runner,
		process: process,
		project: projectDir,
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
// selects the default; clamping happens on the millisecond value so an
// overflow cannot wrap negative and bypass the cap.
func processDeadline(ms int) time.Duration {
	if ms <= 0 {
		return defaultProcessDeadline
	}

	const capMs = int64(maxProcessDeadline / time.Millisecond)
	if int64(ms) >= capMs {
		return maxProcessDeadline
	}

	return time.Duration(ms) * time.Millisecond
}

func (t *bashTool) run(
	ctx context.Context,
	p bashParams,
	workDir string,
	deadline time.Duration,
) (*tool.Result, error) {
	start := time.Now()
	foregroundOnly := deadline < foregroundGrace

	if t.process == nil {
		return t.runDirect(ctx, p, workDir, deadline)
	}

	// Explicit background requests advertise at spawn; automatic promotion
	// starts unadvertised so a command finishing inside the grace never
	// appears in status/progress, and only becomes visible at the boundary.
	spec := backgroundprocess.Spec{
		ProjectDir:    t.project,
		SessionID:     t.session,
		RootSessionID: t.root,
		ToolCallID:    tool.CallIDFromContext(ctx),
		Deadline:      deadline,
		Advertise:     p.Background && !foregroundOnly,
	}

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
		if errors.Is(err, backgroundprocess.ErrSlotLimit) {
			return nil, fmt.Errorf(
				"start process: %w; do not retry with another command. "+
					"Cancel a wrong, stuck, or redundant existing process with cancel_process and its bgp_... ID. "+
					"Do not poll existing processes with Bash, ps, sleep, schedule, Read, or Tail. "+
					"Do not poll with tools; continue only useful independent work. "+
					"When waiting is your only remaining action, reply with a standalone <WAITING/> line and no tool calls",
				err,
			)
		}

		return nil, fmt.Errorf("start process: %w", err)
	}

	if foregroundOnly {
		return t.waitForegroundTerminal(ctx, record, p.Command)
	}

	if p.Background {
		return t.backgroundedResult(record, false), nil
	}

	return t.finishForegroundGrace(ctx, record, p.Command, start.Add(foregroundGrace))
}

func (t *bashTool) finishForegroundGrace(
	ctx context.Context,
	record backgroundprocess.Process,
	command string,
	until time.Time,
) (*tool.Result, error) {
	if result, ok := t.waitForeground(ctx, record, until, command); ok {
		return result, nil
	}

	lifecycleCtx := context.WithoutCancel(ctx)

	advertised, err := t.process.Advertise(lifecycleCtx, record.ID)
	if err != nil {
		return nil, t.abortCandidate(lifecycleCtx, record,
			fmt.Errorf("promote background process: %w", err))
	}

	current, err := t.process.Store().GetProcess(lifecycleCtx, record.ID)

	if err != nil && advertised {
		return t.backgroundedResult(record, true), nil
	}

	if err != nil {
		return nil, t.abortCandidate(lifecycleCtx, record,
			fmt.Errorf("load promoted process: %w", err))
	}

	if !advertised && current.State.Terminal() {
		return t.foregroundResult(ctx, current, command), nil
	}

	return t.backgroundedResult(current, true), nil
}

func (t *bashTool) abortCandidate(
	ctx context.Context,
	record backgroundprocess.Process,
	cause error,
) error {
	_, cancelErr := t.process.CancelProcess(ctx, record.ID, backgroundprocess.IntentDaemonShutdown)
	removeErr := t.process.RemoveOutput(ctx, record.ID)

	return errors.Join(cause, cancelErr, removeErr)
}
