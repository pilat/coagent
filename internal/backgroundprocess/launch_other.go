//go:build !linux

package backgroundprocess

import "syscall"

func guardianProcessSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func commandProcessSysProcAttr(processGroup int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pgid: processGroup}
}
