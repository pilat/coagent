package localapi

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListenLoopback_RefusesANonLoopbackAddress(t *testing.T) {
	for _, address := range []string{
		"0.0.0.0:0", "[::]:0", "192.0.2.1:0", "localhost:0", ":0", "not-an-address", "[::ffff:127.0.0.1]:0",
	} {
		t.Run(address, func(t *testing.T) {
			listener, err := ListenLoopback(t.Context(), address)
			require.ErrorContains(t, err, "not a loopback IP address and port")
			assert.Nil(t, listener)
		})
	}
}

func TestListenLoopback_BindsLoopback(t *testing.T) {
	listener, address := loopbackListener(t)
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, addr.IP.IsLoopback())
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func loopbackListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := ListenLoopback(t.Context(), "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	return listener, listener.Addr().String()
}
