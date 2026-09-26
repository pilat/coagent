//go:build !linux

package sandboxnet

import "errors"

// ErrJoinUnsupported reports that namespace joining needs Linux.
var ErrJoinUnsupported = errors.New("sandbox network join requires Linux")

// RunJoin is unavailable off Linux.
func RunJoin(JoinConfig, []string) error { return ErrJoinUnsupported }
