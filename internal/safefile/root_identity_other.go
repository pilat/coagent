//go:build !linux

package safefile

import "io/fs"

// rootFileIdentity is the generic non-Linux fallback: it reports no platform
// root identity, so New with ProjectConfined fails closed. The Linux-only
// runtime guard is the product boundary that refuses such binaries earlier.
func rootFileIdentity(fs.FileInfo) string { return "" }
