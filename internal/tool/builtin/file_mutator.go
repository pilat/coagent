package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/bashsandbox"
)

const (
	fileMutationOutputLimit = 8 * 1024
	sandboxMutationCommand  = `set -e
if [ "$2" = "1" ]; then
  mkdir -p -- "$3"
fi
cat > "$1"`
)

var (
	_ fileMutator = directFileMutator{}
	_ fileMutator = (*sandboxFileMutator)(nil)
)

// fileMutator replaces file content and optionally creates missing parents.
type fileMutator interface {
	WriteFile(ctx context.Context, path string, content []byte, createParents bool) error
}

type directFileMutator struct{}

type sandboxFileMutator struct {
	runner bashsandbox.Runner
}

type boundedMutationOutput struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	truncated bool
}

func newFileMutator(enabled bool, runner bashsandbox.Runner) (fileMutator, error) {
	if !enabled {
		return directFileMutator{}, nil
	}

	if runner == nil {
		return nil, errors.New("sandbox runner is required")
	}

	return &sandboxFileMutator{runner: runner}, nil
}

func (directFileMutator) WriteFile(
	_ context.Context,
	path string,
	content []byte,
	createParents bool,
) error {
	if createParents {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create parent directories: %w", err)
		}
	}

	if err := rejectNonRegular(path); err != nil {
		return err
	}

	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func (m *sandboxFileMutator) WriteFile(
	ctx context.Context,
	path string,
	content []byte,
	createParents bool,
) error {
	if err := rejectNonRegular(path); err != nil {
		return err
	}

	createParentsArg := "0"
	if createParents {
		createParentsArg = "1"
	}

	cmd, err := m.runner.Command(
		ctx,
		sandboxMutationCommand,
		string(os.PathSeparator),
		"coagent-file-mutator",
		path,
		createParentsArg,
		filepath.Dir(path),
	)
	if err != nil {
		return fmt.Errorf("construct sandboxed file mutation: %w", err)
	}

	configureCommandCancellation(cmd)
	cmd.Stdin = bytes.NewReader(content)

	var output boundedMutationOutput
	cmd.Stdout = &output
	cmd.Stderr = &output

	err = cmd.Run()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err == nil {
		return nil
	}

	detail := strings.TrimSpace(output.String())
	if detail == "" {
		return fmt.Errorf("sandboxed file mutation: %w", err)
	}

	return fmt.Errorf("sandboxed file mutation: %w: %s", err, detail)
}

func (b *boundedMutationOutput) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	originalLen := len(data)
	remaining := fileMutationOutputLimit - b.buffer.Len()

	if remaining <= 0 {
		b.truncated = true
		return originalLen, nil
	}

	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}

	_, _ = b.buffer.Write(data)

	return originalLen, nil
}

func (b *boundedMutationOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	result := b.buffer.String()
	if b.truncated {
		result += "\n... output truncated"
	}

	return result
}
