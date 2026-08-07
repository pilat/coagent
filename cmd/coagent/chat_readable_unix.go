//go:build unix

package main

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether the descriptor has input waiting. Polling keeps the
// terminal single-owner: the loop leaves line mode between reads, not mid-read.
func waitReadable(fd uintptr, wait time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

	for {
		ready, err := unix.Poll(fds, int(wait.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return false, fmt.Errorf("poll input: %w", err)
		}

		return ready > 0, nil
	}
}
