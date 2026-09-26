package sandboxnet

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func testClassifier(t *testing.T, hostPrefixes []string, network []sandboxpolicy.Network) Classifier {
	t.Helper()

	prefixes := make([]netip.Prefix, 0, len(hostPrefixes))
	for _, raw := range hostPrefixes {
		prefixes = append(prefixes, netip.MustParsePrefix(raw))
	}

	classifier, err := NewClassifier(prefixes, network)
	require.NoError(t, err)

	return classifier
}

func TestClassifier_PublicEgressIsAllowedByDefault(t *testing.T) {
	classifier := testClassifier(t, nil, nil)

	for _, address := range []string{"93.184.216.34", "::ffff:93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"} {
		decision := classifier.Allow(netip.MustParseAddr(address), sandboxpolicy.ProtocolTCP, 443)
		assert.True(t, decision.Allowed, address)
		assert.Equal(t, "public destination", decision.Reason, address)
	}
}

func TestClassifier_NonPublicDestinationsNeedAGrant(t *testing.T) {
	classifier := testClassifier(t, []string{"93.184.216.34/32"}, nil)

	tests := map[string]string{
		"loopback":            "127.0.0.1",
		"loopback v6":         "::1",
		"private 10":          "10.1.2.3",
		"private 172":         "172.16.4.4",
		"private 192":         "192.168.1.1",
		"cgnat":               "100.64.0.1",
		"link-local":          "169.254.1.1",
		"zoned link-local v6": "fe80::1%lo",
		"metadata":            "169.254.169.254",
		"metadata v6":         "fd00:ec2::254",
		"unique local v6":     "fd12:3456::1",
		"multicast":           "224.0.0.1",
		"multicast v6":        "ff02::1",
		"reserved":            "240.0.0.1",
		"host interface":      "93.184.216.34",
		"benchmarking":        "198.18.0.1",
		"mapped loopback":     "::ffff:127.0.0.1",
		"unspecified":         "0.0.0.0",
		"unspecified v6":      "::",
		"documentation v6":    "2001:db8::1",
		"teredo":              "2001::1",
		"broadcast reserved":  "255.255.255.255",
		"v4 mapped multicast": "::ffff:224.0.0.1",
	}

	for name, address := range tests {
		t.Run(name, func(t *testing.T) {
			decision := classifier.Allow(netip.MustParseAddr(address), sandboxpolicy.ProtocolTCP, 443)
			assert.False(t, decision.Allowed, address)
			assert.NotEmpty(t, decision.Reason)
			assert.NotEqual(t, "public destination", decision.Reason)
		})
	}
}

func TestClassifier_GrantAdmitsExactlyItsProtocolAndPorts(t *testing.T) {
	classifier := testClassifier(t, nil, []sandboxpolicy.Network{
		{
			Address: "10.0.0.0/8", Protocol: sandboxpolicy.ProtocolTCP,
			Ports: []int{5432}, Type: sandboxpolicy.LevelEscalated,
		},
		{
			Address:  "192.168.5.5",
			Protocol: sandboxpolicy.ProtocolUDP,
			Ports:    []int{53, 5353},
			Type:     sandboxpolicy.LevelBasic,
		},
	})

	allowed := classifier.Allow(netip.MustParseAddr("10.4.4.4"), sandboxpolicy.ProtocolTCP, 5432)
	require.True(t, allowed.Allowed)
	assert.Equal(t, "granted network entry", allowed.Reason)
	assert.Equal(t, "escalated", allowed.Grant)

	assert.True(t, classifier.Allow(netip.MustParseAddr("192.168.5.5"), sandboxpolicy.ProtocolUDP, 5353).Allowed)

	for name, probe := range map[string]struct {
		address  string
		protocol sandboxpolicy.Protocol
		port     int
	}{
		"wrong port":      {"10.4.4.4", sandboxpolicy.ProtocolTCP, 5433},
		"wrong protocol":  {"10.4.4.4", sandboxpolicy.ProtocolUDP, 5432},
		"other private":   {"172.16.4.4", sandboxpolicy.ProtocolTCP, 5432},
		"other host ip":   {"192.168.5.6", sandboxpolicy.ProtocolUDP, 53},
		"granted udp tcp": {"192.168.5.5", sandboxpolicy.ProtocolTCP, 53},
	} {
		t.Run(name, func(t *testing.T) {
			decision := classifier.Allow(netip.MustParseAddr(probe.address), probe.protocol, probe.port)
			assert.False(t, decision.Allowed)
		})
	}
}

func TestClassifier_HostLoopbackNeedsTheReservedAlias(t *testing.T) {
	classifier := testClassifier(t, nil, []sandboxpolicy.Network{
		{
			Address:  sandboxpolicy.HostLoopback,
			Protocol: sandboxpolicy.ProtocolTCP,
			Ports:    []int{8080},
			Type:     sandboxpolicy.LevelEscalated,
		},
	})

	// Ordinary loopback never means the daemon host, even with a host-loopback grant.
	assert.False(t, classifier.Allow(netip.MustParseAddr("127.0.0.1"), sandboxpolicy.ProtocolTCP, 8080).Allowed)
	assert.False(t, classifier.Allow(netip.MustParseAddr("::1"), sandboxpolicy.ProtocolTCP, 8080).Allowed)

	alias := classifier.AllowHostLoopback(sandboxpolicy.ProtocolTCP, 8080)
	require.True(t, alias.Allowed)
	assert.True(t, alias.HostLoopback)
	assert.Equal(t, "escalated", alias.Grant)

	assert.False(t, classifier.AllowHostLoopback(sandboxpolicy.ProtocolTCP, 8081).Allowed)
	assert.False(t, classifier.AllowHostLoopback(sandboxpolicy.ProtocolUDP, 8080).Allowed)
}

func TestClassifier_RejectsMalformedGrant(t *testing.T) {
	_, err := NewClassifier(nil, []sandboxpolicy.Network{
		{Address: "not-an-address", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{80}},
	})
	assert.ErrorContains(t, err, "network grant 0")
}

func TestClassifier_ZeroValueAllowsOnlyPublicEgress(t *testing.T) {
	var classifier Classifier

	assert.True(t, classifier.Allow(netip.MustParseAddr("93.184.216.34"), sandboxpolicy.ProtocolTCP, 443).Allowed)
	assert.False(t, classifier.Allow(netip.MustParseAddr("10.0.0.1"), sandboxpolicy.ProtocolTCP, 443).Allowed)
}

func TestClassifier_ICMPGrantDoesNotGrantTransportPorts(t *testing.T) {
	classifier := testClassifier(t, nil, []sandboxpolicy.Network{
		{Address: "10.0.0.0/8", Protocol: sandboxpolicy.ProtocolICMP, Type: sandboxpolicy.LevelEscalated},
		{Address: "fd00::/8", Protocol: sandboxpolicy.ProtocolICMP, Type: sandboxpolicy.LevelEscalated},
	})
	for _, address := range []string{"10.4.4.4", "fd00::4"} {
		ip := netip.MustParseAddr(address)
		assert.True(t, classifier.Allow(ip, sandboxpolicy.ProtocolICMP, 0).Allowed)
		assert.False(t, classifier.Allow(ip, sandboxpolicy.ProtocolTCP, 443).Allowed)
		assert.False(t, classifier.Allow(ip, sandboxpolicy.ProtocolUDP, 53).Allowed)
	}
	assert.False(t, classifier.Allow(netip.MustParseAddr("192.168.1.1"), sandboxpolicy.ProtocolICMP, 0).Allowed)
}
