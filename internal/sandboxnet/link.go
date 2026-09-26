package sandboxnet

import (
	"net/netip"
	"time"
)

// LinkAddresses are the two ends of the sandbox link: the gateway side lives on
// the router namespace's veth, the sandbox side inside the session's network
// namespace. Both come from one value so they cannot drift.
type LinkAddresses struct {
	GatewayIPv4 netip.Addr
	SandboxIPv4 netip.Addr
	GatewayIPv6 netip.Addr
	SandboxIPv6 netip.Addr

	// HostAliasIPv4 and HostAliasIPv6 are the reserved addresses the sandbox
	// resolves HostAlias to. Only a granted protocol/port pair is translated from
	// them to the daemon host's loopback.
	HostAliasIPv4 netip.Addr
	HostAliasIPv6 netip.Addr
}

// DefaultLink returns the fixed link addressing one network generation uses.
func DefaultLink() LinkAddresses {
	return LinkAddresses{
		GatewayIPv4:   netip.MustParseAddr("10.211.0.1"),
		SandboxIPv4:   netip.MustParseAddr("10.211.0.2"),
		GatewayIPv6:   netip.MustParseAddr("fd00:63:6f:61::1"),
		SandboxIPv6:   netip.MustParseAddr("fd00:63:6f:61::2"),
		HostAliasIPv4: netip.MustParseAddr("10.211.0.3"),
		HostAliasIPv6: netip.MustParseAddr("fd00:63:6f:61::3"),
	}
}

// Config configures one network generation over an already-created namespace.
type Config struct {
	MTU        uint32
	Link       LinkAddresses
	Classifier Classifier

	// MaxInFlight bounds concurrent resolver requests; DNSTimeout bounds each
	// upstream exchange.
	MaxInFlight int
	DNSTimeout  time.Duration

	// DNSUpstreams are the resolvers the generation answers with. Nil means the
	// daemon host's configured resolvers; NoDNS disables the service entirely.
	DNSUpstreams []string
	NoDNS        bool
}
