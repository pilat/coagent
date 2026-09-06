// Package procexec defines the process-construction seam used by sandboxed
// process owners. It deliberately does not prescribe the confinement backend.
package procexec

import (
	"context"
	"errors"
	"os/exec"
)

// Request describes a prepared process without deciding how it is isolated.
type Request struct {
	Path    string
	Args    []string
	WorkDir string
	Env     []string
}

// Runner constructs processes under an execution policy.
//
//nolint:iface // Consumer packages share this process-confinement seam.
type Runner interface {
	Command(ctx context.Context, request Request) (*exec.Cmd, error)
	PolicyKey() string
}

// FromCommand preserves the executable, arguments, environment, and working
// directory chosen by a trusted preparation layer such as shellenv.
func FromCommand(command *exec.Cmd) (Request, error) {
	if command == nil || command.Path == "" || len(command.Args) == 0 {
		return Request{}, errors.New("process command is incomplete")
	}

	var env []string
	if command.Env != nil {
		env = append([]string{}, command.Env...)
	}

	return Request{
		Path:    command.Path,
		Args:    append([]string(nil), command.Args[1:]...),
		WorkDir: command.Dir,
		Env:     env,
	}, nil
}
