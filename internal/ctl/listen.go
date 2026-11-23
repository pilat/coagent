package ctl

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// The kernel reports a path overrun as EINVAL; name the limit before binding.
const maxSocketPath = 100

// ListenUnix binds a same-user socket; only the single-instance lock holder may call it.
func ListenUnix(ctx context.Context, path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf(
			"socket path is %d bytes, over the %d-byte unix limit: %s",
			len(path), maxSocketPath, path,
		)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Safe under the flock: whatever is here is a leftover, not a live daemon.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// A permissive umask would otherwise leave the socket group-writable.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return ln, nil
}
