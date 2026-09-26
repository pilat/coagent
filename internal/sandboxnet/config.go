package sandboxnet

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// PolicyOptions carries the parts of a network configuration that do not come
// from the compiled policy: the link addressing and the operational bounds.
type PolicyOptions struct {
	Link LinkAddresses
	MTU  uint32

	// DNSUpstreams are the resolvers the relay answers with. Nil means the
	// daemon host's configured resolvers.
	DNSUpstreams []string

	MaxInFlight int
	DNSTimeout  time.Duration

	// HostNetworks enumerates the daemon host's attached networks. It is
	// injectable so classification can be tested without the host's interfaces.
	HostNetworks func() ([]netip.Prefix, error)
}

// ConfigFromPolicy turns a compiled policy into the configuration of one
// network generation: the destination classifier built from the policy's
// network grants and the host's own networks. The resolver remains available
// under shields because public egress still needs DNS.
func ConfigFromPolicy(policy sandboxpolicy.Policy, opts PolicyOptions) (Config, error) {
	enumerate := opts.HostNetworks
	if enumerate == nil {
		enumerate = hostNetworks
	}

	prefixes, err := enumerate()
	if err != nil {
		return Config{}, fmt.Errorf("enumerate host networks: %w", err)
	}

	classifier, err := NewClassifier(prefixes, policy.Network)
	if err != nil {
		return Config{}, fmt.Errorf("compile network grants: %w", err)
	}

	return Config{
		Link:         opts.Link,
		MTU:          opts.MTU,
		Classifier:   classifier,
		DNSUpstreams: opts.DNSUpstreams,
		NoDNS:        false,
		MaxInFlight:  opts.MaxInFlight,
		DNSTimeout:   opts.DNSTimeout,
	}, nil
}

// hostNetworks reports the daemon host's attached networks. A destination that
// is one of the host's own addresses is never public egress, even when it is
// globally routable.
func hostNetworks() ([]netip.Prefix, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	prefixes := make([]netip.Prefix, 0, len(interfaces))

	for _, iface := range interfaces {
		addrs, addrErr := iface.Addrs()
		if addrErr != nil {
			return nil, fmt.Errorf("list addresses for interface %s: %w", iface.Name, addrErr)
		}

		for _, addr := range addrs {
			prefix, ok := prefixOf(addr)
			if !ok {
				return nil, fmt.Errorf("unsupported address %v on interface %s", addr, iface.Name)
			}

			prefixes = append(prefixes, prefix)
		}
	}

	return prefixes, nil
}

// prefixOf returns the directly attached network, including the host address.
func prefixOf(addr net.Addr) (netip.Prefix, bool) {
	network, ok := addr.(*net.IPNet)
	if !ok {
		return netip.Prefix{}, false
	}

	parsed, ok := netip.AddrFromSlice(network.IP)
	if !ok {
		return netip.Prefix{}, false
	}

	parsed = parsed.Unmap()

	ones, bits := network.Mask.Size()
	if bits == 128 && parsed.Is4() && ones >= 96 {
		ones -= 96
	}

	if ones < 0 || ones > parsed.BitLen() {
		return netip.Prefix{}, false
	}

	return netip.PrefixFrom(parsed, ones).Masked(), true
}
