//go:build linux

package bashsandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// DialCommand transfers a connection from the tree's private loopback to the
// daemon through a one-use private Unix socket.
const DialCommand = "__coagent_net_dial"

const guestDialSocket = sandboxpolicy.PrivateRuntimeDir + "/dial.sock"

type guestDialCommandRunner interface {
	GuestDialCommand(context.Context, procexec.Request, string) (*exec.Cmd, error)
}

// RunDialMode is a hidden process entry used only after the namespace join.
func RunDialMode(args []string) (bool, error) {
	if len(args) == 0 || args[0] != DialCommand {
		return false, nil
	}

	if len(args) != 5 {
		return true, errors.New("network dial requires socket, token, network and address")
	}

	return true, runGuestDial(args[1], args[2], args[3], args[4])
}

func runGuestDial(socket, token, network, address string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// #nosec G704 -- the joined namespace and its router enforce the destination policy.
	target, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return fmt.Errorf("connect guest loopback: %w", err)
	}
	defer func() { _ = target.Close() }()

	unixConn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("connect handoff socket: %w", err)
	}

	defer func() { _ = unixConn.Close() }()

	if _, err := io.WriteString(unixConn, token); err != nil {
		return fmt.Errorf("write handoff proof: %w", err)
	}

	socketFile, err := unixConn.File()
	if err != nil {
		return fmt.Errorf("open handoff socket descriptor: %w", err)
	}

	defer func() { _ = socketFile.Close() }()

	tcpConn, ok := target.(*net.TCPConn)
	if !ok {
		return errors.New("guest dial returned a non-TCP connection")
	}

	tcpFile, err := tcpConn.File()
	if err != nil {
		return fmt.Errorf("open guest TCP descriptor: %w", err)
	}

	defer func() { _ = tcpFile.Close() }()

	if err := unix.Sendmsg(int(socketFile.Fd()), []byte{'f'}, unix.UnixRights(int(tcpFile.Fd())), nil, 0); err != nil {
		return fmt.Errorf("send guest TCP descriptor: %w", err)
	}

	return nil
}

// DialGuest starts a trusted helper inside the owning runner's namespace and
// receives its connected TCP descriptor. It never opens the daemon's loopback.
func DialGuest(
	ctx context.Context,
	runner procexec.Runner,
	binary, workDir, network, address string,
) (net.Conn, error) {
	commandRunner, ok := runner.(guestDialCommandRunner)
	if !ok {
		return nil, errors.New("guest dial needs a private socket mount")
	}

	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate guest dial proof: %w", err)
	}

	token := hex.EncodeToString(random)
	name := "coagent-dial-" + hex.EncodeToString(random[:8])

	privateRoot, err := coagenthome.Join("network-handoff")
	if err != nil {
		return nil, fmt.Errorf("resolve private guest dial storage: %w", err)
	}

	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create private guest dial storage: %w", err)
	}

	privateDir, err := os.MkdirTemp(privateRoot, "dial-")
	if err != nil {
		return nil, fmt.Errorf("create private guest dial directory: %w", err)
	}

	defer func() { _ = os.Remove(privateDir) }()

	hostSocket := filepath.Join(privateDir, name)
	defer func() { _ = os.Remove(hostSocket) }()

	listener, err := listenPrivateSocket(privateDir, name)
	if err != nil {
		return nil, err
	}

	defer func() { _ = listener.Close(); _ = os.Remove(hostSocket) }()

	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("set guest dial accept deadline: %w", err)
	}

	cmd, err := commandRunner.GuestDialCommand(ctx, procexec.Request{
		Path: binary, Args: []string{DialCommand, guestDialSocket, token, network, address}, WorkDir: workDir,
	}, hostSocket)
	if err != nil {
		return nil, fmt.Errorf("prepare guest dial helper: %w", err)
	}

	if err := cmd.Start(); err != nil {
		for _, file := range cmd.ExtraFiles {
			_ = file.Close()
		}

		return nil, fmt.Errorf("start guest dial: %w", err)
	}

	for _, file := range cmd.ExtraFiles {
		_ = file.Close()
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	return receiveGuestDial(listener, token)
}

func listenPrivateSocket(directory, name string) (*net.UnixListener, error) {
	// Bind through a directory FD: the host path can exceed AF_UNIX's limit.
	dirFD, err := unix.Open(directory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open guest dial directory: %w", err)
	}

	shortSocket := fmt.Sprintf("/proc/self/fd/%d/%s", dirFD, name)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: shortSocket, Net: "unix"})
	_ = unix.Close(dirFD)

	if err != nil {
		return nil, fmt.Errorf("listen for guest dial: %w", err)
	}

	listener.SetUnlinkOnClose(false)

	return listener, nil
}

func receiveGuestDial(listener *net.UnixListener, token string) (net.Conn, error) {
	accepted, err := listener.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("accept guest dial: %w", err)
	}

	defer func() { _ = accepted.Close() }()

	if err := accepted.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("set guest dial read deadline: %w", err)
	}

	proof := make([]byte, len(token))
	if _, err := io.ReadFull(accepted, proof); err != nil {
		return nil, fmt.Errorf("read guest dial proof: %w", err)
	}

	if string(proof) != token {
		return nil, errors.New("guest dial handoff rejected")
	}

	buffer := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))

	_, oobn, _, _, err := accepted.ReadMsgUnix(buffer, oob)
	if err != nil {
		return nil, fmt.Errorf("receive guest socket: %w", err)
	}

	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("parse guest socket control message: %w", err)
	}

	for _, message := range messages {
		fds, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			return nil, fmt.Errorf("parse guest socket rights: %w", parseErr)
		}

		if len(fds) == 0 {
			continue
		}

		file := os.NewFile(uintptr(fds[0]), "guest-loopback")
		conn, convertErr := net.FileConn(file)
		_ = file.Close()

		if convertErr != nil {
			return nil, fmt.Errorf("convert guest TCP descriptor: %w", convertErr)
		}

		return conn, nil
	}

	return nil, errors.New("guest dial returned no descriptor")
}
