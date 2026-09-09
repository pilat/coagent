package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/tool"
)

type bashRunnerStub struct {
	command string
	workDir string
	args    []string
	err     error
	roots   []string
}

func TestBackgroundToolDescriptionsPreventPollingAndDuplicateVerification(t *testing.T) {
	description := bashDescription + backgroundDescriptionSuffix
	assert.GreaterOrEqual(t, strings.Count(strings.ToLower(description), "do not poll"), 3)
	assert.Contains(t, description, "<WAITING/>")
	assert.Contains(t, description, "I_WOULD_USE_<WAITING/>")
	assert.Contains(t, description, "Overlapping builds, test suites, or verification commands")
	assert.Contains(t, tailDescription, "Do not use tail on a running background process")
	assert.Contains(t, tailDescription, "final result arrives automatically in a new turn")
}

func (r *bashRunnerStub) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	r.command = request.Args[1]
	r.workDir = request.WorkDir
	r.args = append([]string(nil), request.Args[2:]...)
	if r.err != nil {
		return nil, r.err
	}

	cmd := exec.CommandContext(ctx, request.Path, request.Args...)
	cmd.Dir = request.WorkDir

	return cmd, nil
}

func (r *bashRunnerStub) BashCommand(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error) {
	return r.Command(
		ctx,
		procexec.Request{Path: "bash", Args: append([]string{"-c", command}, args...), WorkDir: workDir},
	)
}

// ShellCommand mirrors Command: the bash tool calls this path, and tests assert
// on the recorded command/workDir.
func (r *bashRunnerStub) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	return r.BashCommand(ctx, command, workDir)
}

func (r *bashRunnerStub) WritableRoots() []string          { return r.roots }
func (r *bashRunnerStub) PolicyKey() string                { return "stub" }
func (r *bashRunnerStub) ReadScope() bashsandbox.ReadScope { return bashsandbox.HostReadable }

// newTestProcessService builds a real background-process service over a
// migrated temp SQLite database. It returns the service and a cleanup.
func newTestProcessService(t *testing.T) (backgroundprocess.Service, int64) {
	t.Helper()

	ctx := context.Background()

	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "bg.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, migrate.Run(ctx, db, ""))

	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	service := backgroundprocess.NewService(
		backgroundprocess.NewStore(db),
		backgroundprocess.Options{OutputDir: t.TempDir()},
	)
	t.Cleanup(func() {
		_, _ = service.CancelAll(context.Background(), backgroundprocess.IntentSessionKilled)
	})

	return service, 1
}

func newTestBashTool(t *testing.T) (*bashTool, backgroundprocess.Service) {
	t.Helper()

	service, sessionID := newTestProcessService(t)

	return newBashTool(t.TempDir(), &bashRunnerStub{}, service, "project-1", sessionID, sessionID), service
}

// newTestBashToolRunner builds a bash tool over a caller-supplied sandbox
// runner and a real process service.
func newTestBashToolRunner(t *testing.T, workDir string, runner bashsandbox.Runner) *bashTool {
	t.Helper()

	service, sessionID := newTestProcessService(t)

	return newBashTool(workDir, runner, service, "project-1", sessionID, sessionID)
}

func TestBashTool_Execute(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call_test")
	bash, _ := newTestBashTool(t)

	t.Run("simple command", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo hello"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Contains(t, result.Output, "hello")
		assert.Equal(t, 0, result.Metadata["exitCode"])
		assert.False(t, result.IsError)
	})

	t.Run("command with pipes", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo 'hello world' | grep hello"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Contains(t, result.Output, "hello world")
	})

	t.Run("combined stream carries stderr", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo error >&2"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Contains(t, result.Output, "error")
	})

	t.Run("failing command is a typed error", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "exit 1"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.True(t, result.IsError, "foreground nonzero exit must set IsError")
		assert.Equal(t, 1, result.Metadata[metaKeyExitCode])
	})

	t.Run("custom working directory", func(t *testing.T) {
		subDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "test.txt"), []byte("content"), 0o644))

		params, _ := json.Marshal(bashParams{Command: "ls", WorkDir: subDir})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Contains(t, result.Output, "test.txt")
	})

	t.Run("short deadline times out in foreground as an error", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "sleep 5", Timeout: 100})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.True(t, result.IsError)
		assert.Equal(t, true, result.Metadata[metaKeyTimedOut])
		assert.Contains(t, result.Output, "timed out")
	})

	t.Run("short deadline ignores explicit background request", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "sleep 5", Timeout: 100, Background: true})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.True(t, result.IsError)
		assert.Equal(t, true, result.Metadata[metaKeyTimedOut])
		assert.NotContains(t, result.Output, "runs in the background")
	})

	t.Run("empty command", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: ""})
		_, err := bash.Execute(ctx, params)
		require.Error(t, err)
	})

	t.Run("no output", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "true"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Equal(t, "(no output)", result.Output)
	})

	t.Run("environment variables", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo $HOME"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.NotEqual(t, "$HOME", result.Output)
		assert.NotEmpty(t, result.Output)
	})

	t.Run("candidate file removed on inline completion", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo ephemeral"})
		result, err := bash.Execute(ctx, params)
		require.NoError(t, err)

		assert.Contains(t, result.Output, "ephemeral")
		assert.NotContains(t, result.Output, "Output file:", "an inline result never advertises a path")
	})
}

func TestBashTool_ImmediateBackground(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call_test")
	bash, _ := newTestBashTool(t)

	params, _ := json.Marshal(bashParams{Command: "sleep 30", Background: true})
	result, err := bash.Execute(ctx, params)
	require.NoError(t, err)

	assert.Contains(t, result.Output, "do not poll")
	assert.GreaterOrEqual(t, strings.Count(strings.ToLower(result.Output), "do not poll"), 3)
	assert.Contains(t, result.Output, "<WAITING/>")
	assert.Contains(t, result.Output, "no tool calls")
	assert.Contains(t, result.Output, "overlapping command")
	assert.Contains(t, result.Output, "Background execution was requested")
	assert.Contains(t, result.Output, "Background process ID (not an operating-system PID): bgp_")
	assert.Contains(t, result.Output, "cancel_process using this background process ID")
	assert.Contains(t, result.Output, "I_WOULD_USE_<WAITING/>")
	assert.Contains(t, result.Output, "Output file:")

	processID, _ := result.Metadata[metaKeyProcessID].(string)
	require.NotEmpty(t, processID)
}

func TestBashTool_AutomaticPromotion(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call_test")
	bash, _ := newTestBashTool(t)

	start := time.Now()

	params, _ := json.Marshal(bashParams{Command: "sleep 30"})
	result, err := bash.Execute(ctx, params)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, time.Since(start), 9*time.Second, "promotion waits the grace period")
	assert.Contains(t, result.Output, "do not poll")
	assert.GreaterOrEqual(t, strings.Count(strings.ToLower(result.Output), "do not poll"), 3)
	assert.Contains(t, result.Output, "<WAITING/>")
	assert.Contains(t, result.Output, "still running after 10 seconds")
	assert.Contains(t, result.Output, "moved to the background")

	processID, _ := result.Metadata[metaKeyProcessID].(string)
	require.NotEmpty(t, processID)

	// Cleanup: the service owns the process; cancel via tree intent.
	_, _ = bash.process.CancelTree(context.Background(), 1, backgroundprocess.IntentSessionKilled)
}

func TestBashTool_ProcessSlotLimitRedirectsToBackgroundWait(t *testing.T) {
	bash, _ := newTestBashTool(t)
	params, _ := json.Marshal(bashParams{Command: "sleep 30", Background: true})

	for i := range backgroundprocess.LiveProcessLimit {
		ctx := tool.WithCallID(context.Background(), fmt.Sprintf("slot-%d", i))
		_, err := bash.Execute(ctx, params)
		require.NoError(t, err)
	}

	ctx := tool.WithCallID(context.Background(), "slot-overflow")
	_, err := bash.Execute(ctx, params)
	require.ErrorIs(t, err, backgroundprocess.ErrSlotLimit)
	require.ErrorContains(t, err, "do not retry with another command")
	require.ErrorContains(t, err, "cancel_process and its bgp_... ID")
	require.ErrorContains(t, err, "<WAITING/>")
	require.ErrorContains(t, err, "no tool calls")
}

func TestBashTool_Metadata(t *testing.T) {
	bash, _ := newTestBashTool(t)

	if bash.ID() != "bash" {
		t.Errorf("ID should be 'bash', got %s", bash.ID())
	}

	desc := bash.Description()
	if !strings.Contains(desc, "bash") {
		t.Error("Description should mention bash")
	}

	params := bash.Parameters()
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters should be valid JSON: %v", err)
	}
}

func TestBashTool_DelegatesCommandConstruction(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call_test")
	service, sessionID := newTestProcessService(t)
	runner := &bashRunnerStub{}
	workDir := t.TempDir()
	bash := newBashTool(workDir, runner, service, "project-1", sessionID, sessionID)

	params, err := json.Marshal(bashParams{Command: "printf delegated"})
	require.NoError(t, err)

	result, err := bash.Execute(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, "delegated", result.Output)
	assert.Equal(t, "printf delegated", runner.command)
}

func TestBashTool_CommandConstructionError(t *testing.T) {
	service, sessionID := newTestProcessService(t)
	runnerErr := errors.New("sandbox unavailable")
	bash := newBashTool(t.TempDir(), &bashRunnerStub{err: runnerErr}, service, "project-1", sessionID, sessionID)

	params, err := json.Marshal(bashParams{Command: "true"})
	require.NoError(t, err)

	result, err := bash.Execute(context.Background(), params)
	require.ErrorIs(t, err, runnerErr)
	assert.Nil(t, result)
}

func TestProcessDeadlineResolution(t *testing.T) {
	tests := []struct {
		name string
		ms   int
		want time.Duration
	}{
		{name: "omitted selects the default", ms: 0, want: defaultProcessDeadline},
		{name: "negative selects the default", ms: -1, want: defaultProcessDeadline},
		{name: "in-range is honoured", ms: 45000, want: 45 * time.Second},
		{name: "exactly the cap is honoured", ms: int(maxProcessDeadline / time.Millisecond), want: maxProcessDeadline},
		{name: "over the cap is clamped", ms: int(maxProcessDeadline/time.Millisecond) + 1, want: maxProcessDeadline},
		{name: "overflow clamps instead of wrapping negative", ms: 10_000_000_000_000, want: maxProcessDeadline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, processDeadline(tt.ms))
		})
	}
}

var (
	_           = sql.ErrNoRows
	_ tool.Tool = (*bashTool)(nil)
)
