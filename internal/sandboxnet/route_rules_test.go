package sandboxnet

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func testRules(t *testing.T, services []routeService, network ...sandboxpolicy.Network) string {
	t.Helper()

	classifier, err := NewClassifier([]netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, network)
	require.NoError(t, err)

	ruleset, err := (routeRules{
		link: DefaultLink(), classifier: classifier, services: services,
		uplink4:  netip.MustParseAddr("10.212.0.2"),
		uplink6:  netip.MustParseAddr("fd00:63:6f:62::2"),
		transit4: routeTransit4, transit6: routeTransit6,
	}).ruleset()
	require.NoError(t, err)

	return ruleset
}

func dnsServices() []routeService {
	var services []routeService
	for _, protocol := range []sandboxpolicy.Protocol{sandboxpolicy.ProtocolUDP, sandboxpolicy.ProtocolTCP} {
		services = append(services, routeService{
			address: DefaultLink().GatewayIPv4, target: netip.MustParseAddr("10.212.0.1"),
			protocol: protocol, port: dnsPort, targetPort: 40000,
		})
	}

	return services
}

// The guest chain drops by default, so every authority is an explicit accept.
func TestRuleset_DropsEveryDestinationItDoesNotName(t *testing.T) {
	ruleset := testRules(t, nil)

	assert.Contains(t, ruleset, "chain gate { type filter hook prerouting priority -150; policy drop;")
	assert.Contains(t, ruleset, "chain forward { type filter hook forward priority 0; policy drop;")
	assert.Contains(t, ruleset, "chain input { type filter hook input priority 0; policy drop; }")
}

func TestRuleset_PrivateAndHostNetworksAreDroppedWithoutAGrant(t *testing.T) {
	ruleset := testRules(t, nil)

	for _, prefix := range []string{"10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "192.0.2.0/24"} {
		assert.Contains(t, ruleset, " ip daddr "+prefix+" drop\n", prefix+" needs an explicit grant")
	}

	assert.Contains(
		t,
		ruleset,
		" ip daddr "+routeTransit4.String()+" drop\n",
		"transit addressing is not a grantable network",
	)
	assert.Contains(
		t,
		ruleset,
		" ip daddr "+DefaultLink().GatewayIPv4.String()+" drop\n",
		"the router itself is not a destination",
	)
	assert.Contains(t, ruleset, "\n accept\n", "public egress is the fall-through")
}

func TestRuleset_NetworkGrantOpensOnlyItsProtocolAndPort(t *testing.T) {
	ruleset := testRules(t, nil, sandboxpolicy.Network{
		Address: "10.4.0.0/16", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{5432},
	})

	accept := " ip daddr 10.4.0.0/16 tcp dport 5432 accept\n"
	assert.Contains(t, ruleset, accept)
	assert.Less(t, strings.Index(ruleset, accept), strings.Index(ruleset, " ip daddr 10.0.0.0/8 drop\n"),
		"the grant must be read before the private-network drop")
	assert.NotContains(t, ruleset, "udp dport 5432")
}

func TestRuleset_ICMPGrantDoesNotOpenTransportPorts(t *testing.T) {
	ruleset := testRules(t, nil, sandboxpolicy.Network{
		Address: "10.4.0.0/16", Protocol: sandboxpolicy.ProtocolICMP,
	})

	assert.Contains(t, ruleset, " ip daddr 10.4.0.0/16 meta l4proto icmp accept\n")
	assert.NotContains(t, ruleset, "dport")
}

// Translation runs after the policy chain, so a redirect can never authorize a
// destination the guest chain refused.
func TestRuleset_ResolverIsReachableOnlyAtTheGatewayAddress(t *testing.T) {
	ruleset := testRules(t, dnsServices())

	assert.Contains(t, ruleset, " ip daddr "+DefaultLink().GatewayIPv4.String()+" udp dport 53 accept\n")
	assert.Contains(t, ruleset, " dnat ip to 10.212.0.1:40000\n")
	assert.NotContains(t, ruleset, " ip daddr 10.5.5.5 udp dport 53 accept\n",
		"another resolver address stays subject to the private-network drop")
	assert.Less(t, strings.Index(ruleset, "chain guest"), strings.Index(ruleset, "chain translate"))
}

func TestRuleset_WithoutDNSNothingAnswersPort53(t *testing.T) {
	ruleset := testRules(t, nil)

	assert.NotContains(t, ruleset, "dport 53")
	assert.NotContains(t, ruleset, "dnat")
}

func TestRuleset_RejectsAnUnroutableService(t *testing.T) {
	classifier, err := NewClassifier(nil, nil)
	require.NoError(t, err)

	for name, service := range map[string]routeService{
		"mixed family": {
			address: DefaultLink().GatewayIPv4, target: netip.MustParseAddr("fd00::1"),
			protocol: sandboxpolicy.ProtocolTCP, port: 53, targetPort: 53,
		},
		"zoned address": {
			address: netip.MustParseAddr("fe80::1%eth0"), target: netip.MustParseAddr("fe80::2"),
			protocol: sandboxpolicy.ProtocolTCP, port: 53, targetPort: 53,
		},
		"port out of range": {
			address: DefaultLink().GatewayIPv4, target: netip.MustParseAddr("10.212.0.1"),
			protocol: sandboxpolicy.ProtocolTCP, port: 0, targetPort: 53,
		},
		"unknown protocol": {
			address: DefaultLink().GatewayIPv4, target: netip.MustParseAddr("10.212.0.1"),
			protocol: sandboxpolicy.Protocol("sctp"), port: 53, targetPort: 53,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (routeRules{
				link: DefaultLink(), classifier: classifier, services: []routeService{service},
				transit4: routeTransit4, transit6: routeTransit6,
			}).ruleset()
			require.Error(t, err)
		})
	}
}

func TestRuleset_RejectsAnUnroutableGrant(t *testing.T) {
	for name, entry := range map[string]sandboxpolicy.Network{
		"port out of range": {Address: "10.4.0.0/16", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{70000}},
		"unknown protocol":  {Address: "10.4.0.0/16", Protocol: sandboxpolicy.Protocol("sctp"), Ports: []int{443}},
	} {
		t.Run(name, func(t *testing.T) {
			classifier, err := NewClassifier(nil, []sandboxpolicy.Network{entry})
			require.NoError(t, err)

			_, err = (routeRules{
				link: DefaultLink(), classifier: classifier,
				transit4: routeTransit4, transit6: routeTransit6,
			}).ruleset()
			require.Error(t, err)
		})
	}
}
