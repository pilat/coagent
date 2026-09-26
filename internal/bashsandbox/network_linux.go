//go:build linux

package bashsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/sandboxnet"
)

// defaultSetupTimeout bounds the wait for the child's link descriptor.
const defaultSetupTimeout = 30 * time.Second

// userNSDescriptor is the descriptor number bubblewrap receives UserNS on.
const userNSDescriptor = 3

// NetworkSetup is a running namespace-setup child plus the namespace descriptor
// it handed back. Closing the setup socket retires the child, so the caller owns
// its lifetime.
type NetworkSetup struct {
	FD  int
	Cmd *exec.Cmd

	socket int
}

// StartNetworkSetup spawns the setup child, which builds the user and network
// namespaces and returns the network namespace descriptor. Receiving it means
// the namespace is ready for the router to attach.
func StartNetworkSetup(ctx context.Context, cfg NetworkSetupConfig) (*NetworkSetup, error) {
	if cfg.Binary == "" {
		return nil, errors.New("network setup requires the daemon binary path")
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("create setup socket: %w", err)
	}

	child := os.NewFile(uintptr(fds[1]), "setup-socket")

	cmd := exec.CommandContext(ctx, cfg.Binary, sandboxnet.SetupArgs()...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 unix.CLONE_NEWUSER | unix.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}

	if err := cmd.Start(); err != nil {
		_ = child.Close()
		_ = unix.Close(fds[0])

		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
			return nil, fmt.Errorf("%w: %w", sandboxnet.ErrSetupUnsupported, err)
		}

		return nil, fmt.Errorf("start network setup: %w", err)
	}

	_ = child.Close()

	fd, err := receiveSetupFDBounded(fds[0], receiveTimeout(cfg.ReceiveTimeout))
	if err != nil {
		_ = unix.Close(fds[0])

		waited := make(chan error, 1)
		go func() { waited <- cmd.Wait() }()

		var waitErr error
		select {
		case waitErr = <-waited:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			waitErr = <-waited
		}

		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) &&
			exitErr.ExitCode() == sandboxnet.SetupUnsupportedExit {
			return nil, fmt.Errorf("%w: %s", sandboxnet.ErrSetupUnsupported, strings.TrimSpace(stderr.String()))
		}

		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}

		return nil, err
	}

	return &NetworkSetup{FD: fd, Cmd: cmd, socket: fds[0]}, nil
}

// Close releases the descriptors. The process is left alone: it is the confined
// tree, whose lifetime the caller owns.
func (n *NetworkSetup) Close() error {
	if n == nil {
		return nil
	}

	if n.socket >= 0 {
		_ = unix.Close(n.socket)

		n.socket = -1
	}

	if n.FD >= 0 {
		err := unix.Close(n.FD)
		n.FD = -1

		if err != nil {
			return fmt.Errorf("close link descriptor: %w", err)
		}
	}

	return nil
}

func receiveTimeout(configured time.Duration) time.Duration {
	if configured <= 0 {
		return defaultSetupTimeout
	}

	return configured
}

// receiveSetupFDBounded reads the link descriptor under a hard deadline: a child
// that dies without sending one must not hang the daemon, whatever the socket
// timeout option does on this platform.
func receiveSetupFDBounded(socket int, timeout time.Duration) (int, error) {
	ready := []unix.PollFd{{Fd: int32(socket), Events: unix.POLLIN}}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return -1, errors.New("timed out waiting for the link descriptor")
		}

		count, err := unix.Poll(ready, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			return -1, fmt.Errorf("wait for link descriptor: %w", err)
		}

		if count == 0 {
			return -1, errors.New("timed out waiting for the link descriptor")
		}

		return receiveSetupFD(socket)
	}
}

// receiveSetupFD reads the link descriptor from the setup socket.
func receiveSetupFD(socket int) (int, error) {
	payload := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))

	_, oobn, _, _, err := unix.Recvmsg(socket, payload, oob, 0)
	if err != nil {
		return -1, fmt.Errorf("receive link descriptor: %w", err)
	}

	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parse link descriptor: %w", err)
	}

	for _, message := range messages {
		fds, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			return -1, fmt.Errorf("parse link rights: %w", parseErr)
		}

		if len(fds) > 0 {
			return fds[0], nil
		}
	}

	return -1, errors.New("setup child returned no link descriptor")
}
