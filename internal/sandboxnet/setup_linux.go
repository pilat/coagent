//go:build linux

package sandboxnet

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// RunSetup hands the network namespace created by the parent's clone back over
// the setup socket and then holds it. Addressing, routing and policy are the
// router's, applied from the parent through namespace descriptors.
func RunSetup(cfg SetupConfig) error {
	control, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open namespace control socket: %w", err)
	}

	defer func() { _ = unix.Close(control) }()

	if err := setFlags(control, "lo", unix.IFF_UP); err != nil {
		return err
	}

	// Unprivileged ICMP sockets are off by default in a fresh namespace.
	if err := os.WriteFile("/proc/sys/net/ipv4/ping_group_range", []byte("0 0"), 0); err != nil {
		return fmt.Errorf("enable sandbox ping sockets: %w", err)
	}

	namespace, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open sandbox namespace: %w", err)
	}

	defer func() { _ = namespace.Close() }()

	if err := sendFD(cfg.ReturnFD, int(namespace.Fd())); err != nil {
		return err
	}

	return holdNamespaces(cfg.ReturnFD)
}

// holdNamespaces keeps the child alive, inside the namespaces it created, until
// the parent closes the setup socket. That closure is the retirement signal, so
// a crashed parent cannot leave a generation behind for longer than its socket.
func holdNamespaces(socket int) error {
	buffer := make([]byte, 1)

	for {
		read, err := unix.Read(socket, buffer)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}

			return nil
		}

		if read == 0 {
			return nil
		}
	}
}

func setFlags(control int, name string, flags uint16) error {
	req, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("build flags request: %w", err)
	}

	if err := unix.IoctlIfreq(control, unix.SIOCGIFFLAGS, req); err != nil {
		return fmt.Errorf("read interface flags: %w", err)
	}

	req.SetUint16(req.Uint16() | flags)

	if err := unix.IoctlIfreq(control, unix.SIOCSIFFLAGS, req); err != nil {
		return fmt.Errorf("set interface flags: %w", err)
	}

	return nil
}

// sendFD hands the namespace descriptor to the parent over the setup socket.
func sendFD(socket, fd int) error {
	if err := unix.Sendmsg(socket, []byte{'f'}, unix.UnixRights(fd), nil, 0); err != nil {
		return fmt.Errorf("send namespace descriptor: %w", err)
	}

	return nil
}
