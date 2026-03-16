package ctl

import (
	"fmt"

	"github.com/pilat/coagent/internal/coagenthome"
)

// SocketPath returns the default control socket path.
func SocketPath() (string, error) {
	path, err := coagenthome.Join(coagenthome.SocketFileName)
	if err != nil {
		return "", fmt.Errorf("socket path: %w", err)
	}

	return path, nil
}

// LockPath returns the default single-instance lock path.
func LockPath() (string, error) {
	path, err := coagenthome.Join(coagenthome.LockFileName)
	if err != nil {
		return "", fmt.Errorf("lock path: %w", err)
	}

	return path, nil
}
