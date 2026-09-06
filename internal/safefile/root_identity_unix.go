//go:build darwin || linux

package safefile

import (
	"fmt"
	"io/fs"
	"syscall"
)

func rootFileIdentity(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}

	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino)
}
