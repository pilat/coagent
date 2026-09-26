//go:build !linux

package sandboxnet

import "context"

// SweepStaleRules is unavailable outside Linux.
func SweepStaleRules(context.Context) (int, error) { return 0, nil }
