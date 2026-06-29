package builtin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

const (
	webFetchDialTimeout   = 30 * time.Second
	webFetchDialKeepAlive = 30 * time.Second
)

var (
	linkLocalV4Prefix = netip.MustParsePrefix("169.254.0.0/16")
	linkLocalV6Prefix = netip.MustParsePrefix("fe80::/10")
	// NAT64 well-known prefix: an IPv4 destination presented to an IPv6-only host.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// The IPv6 metadata endpoint. Sits in ULA space, so the link-local prefix does not cover it.
	metadataV6Addr = netip.MustParseAddr("fd00:ec2::254")
)

// normalizeFetchAddr canonicalizes a resolved address so the policy sees one shape per destination:
// zone stripped (prefixes never match a zoned Addr), NAT64 unwrapped, 4-in-6 unmapped.
func normalizeFetchAddr(addr netip.Addr) netip.Addr {
	addr = addr.WithZone("")

	if nat64Prefix.Contains(addr) {
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte(b[12:16]))
	}

	return addr.Unmap()
}

func isBlockedFetchAddr(addr netip.Addr) bool {
	return linkLocalV4Prefix.Contains(addr) ||
		linkLocalV6Prefix.Contains(addr) ||
		addr == metadataV6Addr
}

// webFetchDialControl runs after the socket exists but before connect, on the resolved address, so
// hostnames, alternate encodings, redirects and DNS rebinding are all covered by this one check.
func webFetchDialControl(_ context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address %q: %w", address, err)
	}

	if isBlockedFetchAddr(normalizeFetchAddr(ap.Addr())) {
		return fmt.Errorf("destination %s is blocked: link-local or cloud metadata address", ap.Addr())
	}

	return nil
}

// newRestrictedTransport clones the default transport to keep its timeouts. Proxy is nil because a
// proxy would connect on our behalf and make the address check decorative.
func newRestrictedTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:        webFetchDialTimeout,
		KeepAlive:      webFetchDialKeepAlive,
		ControlContext: webFetchDialControl,
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}

	transport := base.Clone()
	transport.DialContext = dialer.DialContext
	transport.Proxy = nil

	return transport
}
