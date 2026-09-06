//go:build !darwin && !linux

package bashsandbox

import (
	"fmt"
	"runtime"
)

func newEnabledRunner(processPolicy) (Runner, error) {
	return nil, fmt.Errorf("Bash sandbox is unsupported on %s", runtime.GOOS)
}
