package sandboxnet

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// HostAlias is the reserved name a sandboxed client uses to reach a granted
// host-loopback service. Ordinary loopback inside the sandbox is the tree's own
// and never means the daemon host.
const HostAlias = "host.coagent.internal"

// Decision is the classification of one dialed destination.
type Decision struct {
	Allowed bool

	// Reason names the classification that produced the decision. It never
	// contains payload or credential material.
	Reason string

	// Grant names the profile whose network entry admitted the destination; it is
	// empty for public egress.
	Grant string

	// HostLoopback marks a destination that must be translated to the daemon
	// host's loopback: only a granted HostAlias destination sets it.
	HostLoopback bool
}

// nonPublicPrefixes are the ranges that need an explicit grant: loopback,
// private, CGNAT, link-local, multicast, benchmarking, documentation and
// reserved space.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type networkGrant struct {
	prefix       netip.Prefix
	protocol     sandboxpolicy.Protocol
	ports        []int
	profile      string
	hostLoopback bool
}

// Classifier decides one connection against the host's own interfaces and the
// operator's network grants. A zero Classifier allows only public destinations.
type Classifier struct {
	hostPrefixes []netip.Prefix
	grants       []networkGrant
}

// NewClassifier compiles the host's attached networks and the policy's network
// entries. hostPrefixes must be the daemon host's interface addresses: a host
// address is never public egress, even when it is globally routable.
func NewClassifier(hostPrefixes []netip.Prefix, network []sandboxpolicy.Network) (Classifier, error) {
	grants := make([]networkGrant, 0, len(network))

	for i, entry := range network {
		grant, err := compileGrant(entry)
		if err != nil {
			return Classifier{}, fmt.Errorf("network grant %d: %w", i, err)
		}

		grants = append(grants, grant)
	}

	return Classifier{hostPrefixes: hostPrefixes, grants: grants}, nil
}

func compileGrant(entry sandboxpolicy.Network) (networkGrant, error) {
	grant := networkGrant{
		protocol: entry.Protocol, ports: append([]int(nil), entry.Ports...), profile: string(entry.Type),
	}
	slices.Sort(grant.ports)

	if entry.Address == sandboxpolicy.HostLoopback {
		grant.hostLoopback = true

		return grant, nil
	}

	if strings.Contains(entry.Address, "/") {
		prefix, err := netip.ParsePrefix(entry.Address)
		if err != nil {
			return networkGrant{}, fmt.Errorf("parse prefix %q: %w", entry.Address, err)
		}

		grant.prefix = prefix.Masked()

		return grant, nil
	}

	address, err := netip.ParseAddr(entry.Address)
	if err != nil {
		return networkGrant{}, fmt.Errorf("parse address %q: %w", entry.Address, err)
	}

	grant.prefix = netip.PrefixFrom(address.Unmap(), address.Unmap().BitLen())

	return grant, nil
}

// Allow classifies one destination. The address is matched as the kernel
// delivered it, so an IPv4-mapped IPv6 destination is classified as IPv4.
func (c Classifier) Allow(destination netip.Addr, protocol sandboxpolicy.Protocol, port int) Decision {
	address := destination.WithZone("").Unmap()
	if !address.IsValid() {
		return Decision{Reason: "invalid destination"}
	}

	if address.Is4In6() {
		address = address.Unmap()
	}

	if grant, ok := c.matchingGrant(address, protocol, port); ok {
		return Decision{
			Allowed:      true,
			Reason:       "granted network entry",
			Grant:        grant.profile,
			HostLoopback: grant.hostLoopback,
		}
	}

	if c.isHostInterface(address) {
		return Decision{Reason: "daemon host attached network"}
	}

	if class, ok := classifyNonPublic(address); ok {
		return Decision{Reason: class}
	}

	return Decision{Allowed: true, Reason: "public destination"}
}

// AllowHostLoopback classifies a destination that arrived through the reserved
// host alias, where only the granted protocol/port pairs may be translated to
// the daemon host's loopback.
func (c Classifier) AllowHostLoopback(protocol sandboxpolicy.Protocol, port int) Decision {
	for _, grant := range c.grants {
		if !grant.hostLoopback || !grant.matchesTransport(protocol, port) {
			continue
		}

		return Decision{Allowed: true, Reason: "granted host-loopback entry", Grant: grant.profile, HostLoopback: true}
	}

	return Decision{Reason: "host loopback is not granted"}
}

// matchingGrant returns the grant admitting a destination. A host-loopback grant
// is matched against the reserved alias, not against ordinary loopback, so a
// sandbox client cannot reach the daemon host by writing 127.0.0.1.
func (c Classifier) matchingGrant(address netip.Addr, protocol sandboxpolicy.Protocol, port int) (networkGrant, bool) {
	for _, grant := range c.grants {
		if !grant.matchesTransport(protocol, port) {
			continue
		}

		if grant.hostLoopback {
			continue
		}

		if grant.prefix.Contains(address) {
			return grant, true
		}
	}

	return networkGrant{}, false
}

func (c Classifier) isHostInterface(address netip.Addr) bool {
	for _, prefix := range c.hostPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}

	return false
}

func (g networkGrant) matchesTransport(protocol sandboxpolicy.Protocol, port int) bool {
	return g.protocol == protocol && (protocol == sandboxpolicy.ProtocolICMP || slices.Contains(g.ports, port))
}

func classifyNonPublic(address netip.Addr) (string, bool) {
	for _, prefix := range nonPublicPrefixes {
		if !prefix.Contains(address) {
			continue
		}

		switch {
		case prefix.Addr().IsLoopback():
			return "loopback destination", true
		case prefix.Addr().IsMulticast():
			return "multicast destination", true
		case prefix.Addr().IsLinkLocalUnicast():
			return "link-local destination", true
		default:
			return "non-public destination " + prefix.String(), true
		}
	}

	return "", false
}
