//go:build !linux

package procexec

import "os/exec"

func Unprivileged(command *exec.Cmd) *exec.Cmd { return command }
