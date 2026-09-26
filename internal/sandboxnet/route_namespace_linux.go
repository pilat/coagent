//go:build linux

package sandboxnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const routeCommandTimeout = 5 * time.Second

// Namespace changes are thread-local. A failed restoration discards the locked
// thread instead of returning a foreign namespace to the runtime's thread pool.
func inRouteNamespace(fd int, action func() error) error {
	result := make(chan error, 1)

	go func() {
		runtime.LockOSThread()

		original, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()

			result <- fmt.Errorf("pin current network namespace: %w", err)

			return
		}

		defer func() { _ = unix.Close(original) }()
		defer func() {
			if restored := unix.Setns(original, unix.CLONE_NEWNET); restored != nil {
				err = errors.Join(err, fmt.Errorf("restore network namespace: %w", restored))
			} else {
				runtime.UnlockOSThread()
			}

			result <- err
		}()

		if fd >= 0 {
			if err = unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
				err = fmt.Errorf("enter network namespace: %w", err)
				return
			}
		}

		err = action()
	}()

	return <-result
}

func newRouteNamespace() (*os.File, error) {
	var namespace *os.File

	err := inRouteNamespace(-1, func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("create router namespace (requires network administration privileges): %w", err)
		}
		var err error

		namespace, err = os.Open("/proc/thread-self/ns/net")
		if err != nil {
			return fmt.Errorf("pin router namespace: %w", err)
		}

		return nil
	})
	if err != nil && namespace != nil {
		_ = namespace.Close()
	}

	return namespace, err
}

func applyRouteRules(ctx context.Context, namespace int, rules string) error {
	return inRouteNamespace(namespace, func() error {
		commandCtx, cancel := context.WithTimeout(ctx, routeCommandTimeout)
		defer cancel()

		command := exec.CommandContext(commandCtx, "nft", "-f", "-")
		command.Stdin = strings.NewReader(rules)
		command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("apply sandbox routing policy: %w: %s", err, strings.TrimSpace(string(output)))
		}

		return nil
	})
}

func configureRouterKernel(namespace int) error {
	return inRouteNamespace(namespace, func() error {
		for path, value := range map[string]string{
			"/proc/sys/net/ipv4/ip_forward":                       "1",
			"/proc/sys/net/ipv6/conf/all/forwarding":              "1",
			"/proc/sys/net/ipv4/conf/all/accept_redirects":        "0",
			"/proc/sys/net/ipv4/conf/default/accept_redirects":    "0",
			"/proc/sys/net/ipv4/conf/all/accept_source_route":     "0",
			"/proc/sys/net/ipv4/conf/default/accept_source_route": "0",
			"/proc/sys/net/ipv6/conf/all/accept_redirects":        "0",
			"/proc/sys/net/ipv6/conf/default/accept_redirects":    "0",
			"/proc/sys/net/ipv6/conf/all/accept_source_route":     "-1",
			"/proc/sys/net/ipv6/conf/default/accept_source_route": "-1",
		} {
			if err := os.WriteFile(path, []byte(value), 0); err != nil {
				return fmt.Errorf("configure router %s: %w", path, err)
			}
		}

		return nil
	})
}

func checkHostForwarding() error {
	for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv6/conf/all/forwarding"} {
		value, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read host forwarding: %w", err)
		}

		if strings.TrimSpace(string(value)) != "1" {
			return fmt.Errorf("sandbox routing requires %s=1; configure host forwarding before starting coagent", path)
		}
	}

	return nil
}
