//go:build linux

package sandboxnet

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func (l *routeLinks) hostRules(classifier Classifier) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s%s {\n", hostTablePrefix, l.name)
	b.WriteString(" chain translate { type nat hook prerouting priority dstnat; policy accept;\n")

	for _, grant := range classifier.grants {
		if !grant.hostLoopback || grant.protocol == sandboxpolicy.ProtocolICMP {
			continue
		}

		for _, port := range grant.ports {
			fmt.Fprintf(
				&b,
				" iifname %q ip daddr %s %s dport %d dnat ip to 127.0.0.1\n",
				l.name,
				l.host4,
				grant.protocol,
				port,
			)
			fmt.Fprintf(
				&b,
				" iifname %q ip6 daddr %s %s dport %d dnat ip6 to ::1\n",
				l.name,
				l.host6,
				grant.protocol,
				port,
			)
		}
	}

	b.WriteString(" }\n chain conceal { type filter hook prerouting priority 0; policy accept;\n")
	// IPv6 strict route lookup rejects ::1 arriving on veth. Restore it only
	// after local delivery has been selected for the granted DNAT connection.
	fmt.Fprintf(
		&b,
		" iifname %q ct status dnat ct original ip6 daddr %s ip6 daddr ::1 ip6 daddr set %s\n",
		l.name,
		l.host6,
		l.host6,
	)
	b.WriteString(" }\n chain restore { type filter hook input priority -10; policy accept;\n")
	fmt.Fprintf(
		&b,
		" iifname %q ct status dnat ct original ip6 daddr %s ip6 daddr %s ip6 daddr set ::1\n",
		l.name,
		l.host6,
		l.host6,
	)
	b.WriteString(" }\n chain source { type nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, " iifname %q ip saddr %s masquerade\n", l.name, l.router4)
	fmt.Fprintf(&b, " iifname %q ip6 saddr %s masquerade\n", l.name, l.router6)
	b.WriteString(" }\n}\n")

	return b.String()
}

func (l *routeLinks) loopbackServices(link LinkAddresses, classifier Classifier) []routeService {
	var services []routeService

	for _, grant := range classifier.grants {
		if !grant.hostLoopback {
			continue
		}

		ports := grant.ports
		if grant.protocol == sandboxpolicy.ProtocolICMP {
			ports = []int{0}
		}

		for _, port := range ports {
			for _, pair := range [][2]netip.Addr{{link.HostAliasIPv4, l.host4}, {link.HostAliasIPv6, l.host6}} {
				services = append(services, routeService{
					address: pair[0], target: pair[1], protocol: grant.protocol, port: port, targetPort: port,
				})
			}
		}
	}

	return services
}
