package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bashRunnerStub struct {
	command string
	workDir string
	args    []string
	err     error
	roots   []string
}

func (r *bashRunnerStub) Command(
	ctx context.Context,
	command, workDir string,
	args ...string,
) (*exec.Cmd, error) {
	r.command = command
	r.workDir = workDir
	r.args = append([]string(nil), args...)
	if r.err != nil {
		return nil, r.err
	}

	commandArgs := append([]string{"-c", command}, args...)
	cmd := exec.CommandContext(ctx, "bash", commandArgs...)
	cmd.Dir = workDir

	return cmd, nil
}

// ShellCommand mirrors Command: the bash tool calls this path, and tests assert
// on the recorded command/workDir.
func (r *bashRunnerStub) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	return r.Command(ctx, command, workDir)
}

func (r *bashRunnerStub) WritableRoots() []string { return r.roots }

func TestBashTool_Execute(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bash_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tool := newBashTool(tmpDir, &bashRunnerStub{})

	t.Run("simple command", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo hello"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "hello") {
			t.Errorf("Output should contain 'hello', got %s", result.Output)
		}
		if result.Metadata["exitCode"] != 0 {
			t.Errorf("Exit code should be 0, got %v", result.Metadata["exitCode"])
		}
	})

	t.Run("command with pipes", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo 'hello world' | grep hello"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "hello world") {
			t.Errorf("Output should contain 'hello world', got %s", result.Output)
		}
	})

	t.Run("command with stderr", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo error >&2"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "[stderr]") {
			t.Error("Output should include stderr marker")
		}
		if !strings.Contains(result.Output, "error") {
			t.Error("Output should include stderr content")
		}
	})

	t.Run("failing command", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "exit 1"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute should not error on non-zero exit: %v", err)
		}

		if result.Metadata["exitCode"] != 1 {
			t.Errorf("Exit code should be 1, got %v", result.Metadata["exitCode"])
		}
	})

	t.Run("custom working directory", func(t *testing.T) {
		subDir := tmpDir + "/subdir"
		if err := os.Mkdir(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(subDir+"/test.txt", []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}

		params, _ := json.Marshal(bashParams{
			Command: "ls",
			WorkDir: subDir,
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !strings.Contains(result.Output, "test.txt") {
			t.Errorf("Should list files in custom directory, got %s", result.Output)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{
			Command: "sleep 5",
			Timeout: 100, // 100ms
		})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute should not error on timeout: %v", err)
		}

		if result.Metadata["timedOut"] != true {
			t.Error("Should indicate timeout")
		}
		if !strings.Contains(result.Output, "timed out") {
			t.Error("Output should mention timeout")
		}
	})

	t.Run("empty command", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: ""})
		_, err := tool.Execute(context.Background(), params)
		if err == nil {
			t.Fatal("Expected error for empty command")
		}
	})

	t.Run("no output", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "true"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if result.Output != "(no output)" {
			t.Errorf("Empty output should show '(no output)', got %q", result.Output)
		}
	})

	t.Run("environment variables", func(t *testing.T) {
		params, _ := json.Marshal(bashParams{Command: "echo $HOME"})
		result, err := tool.Execute(context.Background(), params)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if result.Output == "$HOME" || result.Output == "" {
			t.Error("Should expand environment variables")
		}
	})
}

func TestBashTool_Metadata(t *testing.T) {
	tool := newBashTool("/tmp", &bashRunnerStub{})

	if tool.ID() != "bash" {
		t.Errorf("ID should be 'bash', got %s", tool.ID())
	}

	desc := tool.Description()
	if !strings.Contains(desc, "bash") {
		t.Error("Description should mention bash")
	}

	params := tool.Parameters()
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters should be valid JSON: %v", err)
	}
}

func TestBashTool_DelegatesCommandConstruction(t *testing.T) {
	runner := &bashRunnerStub{}
	workDir := t.TempDir()
	tool := newBashTool("/unused", runner)
	params, err := json.Marshal(bashParams{Command: "printf delegated", WorkDir: workDir})
	require.NoError(t, err)

	result, err := tool.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, "delegated", result.Output)
	assert.Equal(t, "printf delegated", runner.command)
	assert.Equal(t, workDir, runner.workDir)
}

func TestBashTool_CommandConstructionError(t *testing.T) {
	runnerErr := errors.New("sandbox unavailable")
	tool := newBashTool(t.TempDir(), &bashRunnerStub{err: runnerErr})
	params, err := json.Marshal(bashParams{Command: "true"})
	require.NoError(t, err)

	result, err := tool.Execute(context.Background(), params)
	require.ErrorIs(t, err, runnerErr)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "create bash command")
}
