//go:build !linux

package bashsandbox

import (
	"context"
	"errors"
	"os/exec"
)

// NetworkSetup is unavailable off Linux.
type NetworkSetup struct {
	FD  int
	Cmd *exec.Cmd
}

// StartNetworkSetup is unavailable off Linux.
func StartNetworkSetup(context.Context, NetworkSetupConfig) (*NetworkSetup, error) {
	return nil, errors.New("sandbox network setup requires Linux")
}

// Close is a no-op off Linux.
func (*NetworkSetup) Close() error { return nil }
