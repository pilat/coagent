package localapi

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// ListenLoopback refuses addresses other than loopback IP literals before binding.
func ListenLoopback(ctx context.Context, address string) (net.Listener, error) {
	parsed, err := netip.ParseAddrPort(address)
	if err != nil || !parsed.Addr().IsLoopback() || parsed.Addr().Is4In6() {
		return nil, fmt.Errorf("refusing to listen on %q: not a loopback IP address and port", address)
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on loopback %q: %w", address, err)
	}

	return listener, nil
}
