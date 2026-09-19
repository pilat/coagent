//go:build !linux

package bashsandbox

import (
	"fmt"
	"runtime"
)

// newEnabledRunner is the generic non-Linux fallback. It exists only so the
// package compiles where its Unix-oriented build structure requires it; any
// enabled-sandbox construction on a non-Linux host returns an
// unsupported-backend error. The Linux-only runtime guard is the product
// boundary that refuses such binaries before they reach this code.
func newEnabledRunner(processPolicy) (Runner, error) {
	return nil, fmt.Errorf("Bash sandbox is unsupported on %s", runtime.GOOS)
}
