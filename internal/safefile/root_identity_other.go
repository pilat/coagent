//go:build !darwin && !linux

package safefile

import "io/fs"

func rootFileIdentity(fs.FileInfo) string { return "" }
