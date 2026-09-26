package sandboxnet

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

const (
	routerGuestLink = "guest0"
	routerUplink    = "uplink0"
	routerTable     = "coagent"

	// hostTablePrefix names one generation's host rules. The interface it
	// carries is what lets a later sweep tell a stale table from a live one.
	hostTablePrefix = routerTable + "_"
)

type routeService struct {
	address    netip.Addr
	protocol   sandboxpolicy.Protocol
	port       int
	target     netip.Addr
	targetPort int
}

type routeRules struct {
	link       LinkAddresses
	classifier Classifier
	services   []routeService
	uplink4    netip.Addr
	uplink6    netip.Addr
	transit4   netip.Prefix
	transit6   netip.Prefix
}

// ruleset belongs exclusively to the root-owned router namespace. Policy runs
// before DNAT, so a service translation cannot authorize another destination.
func (r routeRules) ruleset() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", routerTable)
	b.WriteString(" chain gate { type filter hook prerouting priority -150; policy drop;\n")
	fmt.Fprintf(&b, " iifname %q ip saddr %s jump guest\n", routerGuestLink, r.link.SandboxIPv4)
	fmt.Fprintf(&b, " iifname %q ip6 saddr %s jump guest\n", routerGuestLink, r.link.SandboxIPv6)
	fmt.Fprintf(&b, " iifname %q ct state established,related accept\n", routerUplink)
	b.WriteString(" }\n chain guest {\n ct state invalid drop\n")

	for _, service := range r.services {
		if err := service.validate(); err != nil {
			return "", err
		}

		fmt.Fprintf(&b, " %s daddr %s %s accept\n",
			ipFamily(service.address), service.address, service.match())
	}
	// Transit addresses are infrastructure, never ordinary private-network grants.
	for _, prefix := range []netip.Prefix{r.transit4, r.transit6} {
		fmt.Fprintf(&b, " %s daddr %s drop\n", ipFamily(prefix.Addr()), prefix)
	}

	for _, address := range []netip.Addr{r.link.GatewayIPv4, r.link.GatewayIPv6, r.link.HostAliasIPv4, r.link.HostAliasIPv6} {
		fmt.Fprintf(&b, " %s daddr %s drop\n", ipFamily(address), address)
	}

	for _, grant := range r.classifier.grants {
		if grant.hostLoopback {
			continue
		}

		if err := writeRouteGrant(&b, grant); err != nil {
			return "", err
		}
	}

	for _, prefix := range append(append([]netip.Prefix(nil), nonPublicPrefixes...), r.classifier.hostPrefixes...) {
		fmt.Fprintf(&b, " %s daddr %s drop\n", ipFamily(prefix.Addr()), prefix.Masked())
	}

	b.WriteString(" accept\n }\n")
	b.WriteString(" chain input { type filter hook input priority 0; policy drop; }\n")
	b.WriteString(" chain forward { type filter hook forward priority 0; policy drop;\n")
	fmt.Fprintf(&b, " iifname %q oifname %q accept\n", routerGuestLink, routerUplink)
	fmt.Fprintf(&b, " iifname %q oifname %q ct state established,related accept\n", routerUplink, routerGuestLink)
	b.WriteString(" }\n chain output { type filter hook output priority 0; policy accept; }\n")
	b.WriteString(" chain translate { type nat hook prerouting priority dstnat; policy accept;\n")

	for _, service := range r.services {
		target := service.target.String() + ":" + strconv.Itoa(service.targetPort)
		if service.target.Is6() {
			target = "[" + service.target.String() + "]:" + strconv.Itoa(service.targetPort)
		}

		if service.protocol == sandboxpolicy.ProtocolICMP {
			target = service.target.String()
		}

		fmt.Fprintf(&b, " iifname %q %s daddr %s %s dnat %s to %s\n",
			routerGuestLink, ipFamily(service.address), service.address, service.match(),
			ipFamily(service.target), target)
	}

	b.WriteString(" }\n chain source { type nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, " oifname %q ip saddr %s snat ip to %s\n", routerUplink, r.link.SandboxIPv4, r.uplink4)
	fmt.Fprintf(&b, " oifname %q ip6 saddr %s snat ip6 to %s\n", routerUplink, r.link.SandboxIPv6, r.uplink6)
	b.WriteString(" }\n}\n")

	return b.String(), nil
}

func (s routeService) validate() error {
	if !s.address.IsValid() || !s.target.IsValid() || s.address.Is4() != s.target.Is4() ||
		s.address.Zone() != "" || s.target.Zone() != "" {
		return errors.New("invalid routed service addresses")
	}

	if s.protocol == sandboxpolicy.ProtocolICMP && s.port == 0 && s.targetPort == 0 {
		return nil
	}

	if s.protocol != sandboxpolicy.ProtocolTCP && s.protocol != sandboxpolicy.ProtocolUDP {
		return fmt.Errorf("invalid routed service protocol %q", s.protocol)
	}

	if s.port < 1 || s.port > 65535 || s.targetPort < 1 || s.targetPort > 65535 {
		return errors.New("invalid routed service ports")
	}

	return nil
}

func (s routeService) match() string {
	if s.protocol == sandboxpolicy.ProtocolICMP {
		if s.address.Is4() {
			return "meta l4proto icmp"
		}

		return "meta l4proto ipv6-icmp"
	}

	return fmt.Sprintf("%s dport %d", s.protocol, s.port)
}

func writeRouteGrant(b *strings.Builder, grant networkGrant) error {
	if !grant.prefix.IsValid() || grant.prefix.Addr().Is4In6() {
		return fmt.Errorf("invalid routed network prefix %s", grant.prefix)
	}

	if grant.protocol == sandboxpolicy.ProtocolICMP {
		protocol := "icmp"
		if grant.prefix.Addr().Is6() {
			protocol = "ipv6-icmp"
		}

		fmt.Fprintf(b, " %s daddr %s meta l4proto %s accept\n", ipFamily(grant.prefix.Addr()), grant.prefix, protocol)

		return nil
	}

	if grant.protocol != sandboxpolicy.ProtocolTCP && grant.protocol != sandboxpolicy.ProtocolUDP {
		return fmt.Errorf("invalid routed network protocol %q", grant.protocol)
	}

	for _, port := range grant.ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid routed network port %d", port)
		}

		fmt.Fprintf(
			b,
			" %s daddr %s %s dport %d accept\n",
			ipFamily(grant.prefix.Addr()),
			grant.prefix,
			grant.protocol,
			port,
		)
	}

	return nil
}

func ipFamily(address netip.Addr) string {
	if address.Is4() {
		return "ip"
	}

	return "ip6"
}
