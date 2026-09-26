package procexec

import "os/exec"

// CloseExtraFiles releases launcher-side descriptors after Start or Run has
// returned; the child already inherited its own copies by then.
func CloseExtraFiles(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}

	cmd.ExtraFiles = nil
}
