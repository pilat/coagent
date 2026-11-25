package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

type hookFixture struct {
	home string
	dir  string
	ctx  context.Context
	logs *observer.ObservedLogs
}

type hookTool struct {
	id   string
	name tool.HookName
}

func (t *hookTool) ID() string                { return t.id }
func (*hookTool) Description() string         { return "hook fixture" }
func (*hookTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (*hookTool) ParallelSafe() bool          { return false }
func (*hookTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	return &tool.Result{Output: "executed"}, nil
}
func (t *hookTool) HookName() tool.HookName { return t.name }

func newHookFixture(t *testing.T) hookFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	core, logs := observer.New(zap.DebugLevel)
	return hookFixture{
		home: home,
		dir:  t.TempDir(),
		ctx:  logger.ToContext(context.Background(), zap.New(core)),
		logs: logs,
	}
}

func (f hookFixture) launch(ctx context.Context, process Process) (Outcome, error) {
	cmd := exec.CommandContext(ctx, process.Argv[0], process.Argv[1:]...)
	cmd.Env = append(os.Environ(), process.Env...)
	cmd.Dir = f.dir
	cmd.Stdin = bytes.NewReader(process.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outcome := Outcome{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		return outcome, ctx.Err()
	}
	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		outcome.ExitCode = exitError.ExitCode()
		return outcome, nil
	}
	return outcome, err
}

func (f hookFixture) load(t *testing.T) *Hooks {
	t.Helper()
	hooks, err := Load(f.ctx, f.dir, 42, f.launch)
	require.NoError(t, err)
	return hooks
}

func (f hookFixture) settings(t *testing.T, location string, settings map[string]any) {
	t.Helper()
	path := filepath.Join(f.dir, ".claude", location)
	if location == "global" {
		path = filepath.Join(f.home, ".coagent", "settings.json")
	}
	if location == "claude-home" {
		path = filepath.Join(f.home, ".claude", "settings.json")
	}
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	writeHookFile(t, path, data)
}

func (f hookFixture) command(t *testing.T, handler map[string]any) *Hooks {
	t.Helper()
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", handler))
	hooks := f.load(t)
	require.NotNil(t, hooks)
	return hooks
}

func hookSettings(event, matcher string, handlers ...map[string]any) map[string]any {
	return map[string]any{"hooks": map[string]any{event: []any{
		map[string]any{"matcher": matcher, "hooks": handlers},
	}}}
}

func commandHandler(command string) map[string]any {
	return map[string]any{"type": "command", "command": command}
}

func hookCall(id, name string, input string) tool.Call {
	return tool.Call{
		Name:      id,
		Tool:      &hookTool{id: id, name: tool.HookName{Name: name}},
		Arguments: json.RawMessage(input),
	}
}

func bashCall() tool.Call { return hookCall("bash", "Bash", `{"command":"echo hello"}`) }

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func jsonCommand(value string) string { return "printf '%s' " + shellQuote(value) }

func writeHookFile(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func readHookPayload(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(data, &payload))
	return payload
}
