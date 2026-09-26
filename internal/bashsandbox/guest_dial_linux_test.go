//go:build linux

package bashsandbox

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
)

type guestDialRunner struct{}

func (r guestDialRunner) GuestDialCommand(
	ctx context.Context, request procexec.Request, socket string,
) (*exec.Cmd, error) {
	request.Args[1] = socket

	return r.Command(ctx, request)
}

func (guestDialRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	// #nosec G702 -- the test supplies its own executable and fixed helper arguments.
	return exec.CommandContext(ctx, os.Args[0], request.Args...), nil
}

func (guestDialRunner) PolicyKey() string { return "guest-dial-test" }

func TestDialGuest_TransfersGuestLoopbackConnection(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "cdt-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Cleanup(coagenthome.Override(home))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "guest")
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := DialGuest(ctx, guestDialRunner{}, os.Args[0], "/tmp", "tcp", listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	data := make([]byte, 5)
	_, err = io.ReadFull(conn, data)
	require.NoError(t, err)
	assert.Equal(t, "guest", string(data))
}

func TestListenPrivateSocketLongPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("nested-", 15))
	require.NoError(t, os.MkdirAll(directory, 0o700))
	name := "coagent-dial-test"
	require.GreaterOrEqual(t, len(filepath.Join(directory, name)), 108)

	listener, err := listenPrivateSocket(directory, name)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	_, err = os.Stat(filepath.Join(directory, name))
	require.NoError(t, err)
}
