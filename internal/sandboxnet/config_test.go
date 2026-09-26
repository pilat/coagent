package sandboxnet

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func policyOptions(prefixes ...string) PolicyOptions {
	hosts := make([]netip.Prefix, 0, len(prefixes))
	for _, raw := range prefixes {
		hosts = append(hosts, netip.MustParsePrefix(raw))
	}

	return PolicyOptions{
		Link:         DefaultLink(),
		HostNetworks: func() ([]netip.Prefix, error) { return hosts, nil },
	}
}

func TestConfigFromPolicy_ClassifierFollowsTheNetworkGrants(t *testing.T) {
	policy := sandboxpolicy.Policy{
		Network: []sandboxpolicy.Network{{
			Address: "10.0.0.0/8", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{5432},
			Type: sandboxpolicy.LevelEscalated,
		}},
	}

	config, err := ConfigFromPolicy(policy, policyOptions())
	require.NoError(t, err)

	assert.True(t, config.Classifier.Allow(netip.MustParseAddr("10.4.4.4"), sandboxpolicy.ProtocolTCP, 5432).Allowed)
	assert.False(t, config.Classifier.Allow(netip.MustParseAddr("10.4.4.4"), sandboxpolicy.ProtocolTCP, 5433).Allowed)
	assert.True(
		t,
		config.Classifier.Allow(netip.MustParseAddr("93.184.216.34"), sandboxpolicy.ProtocolTCP, 443).Allowed,
	)
}

func TestConfigFromPolicy_ShieldsRetainTheResolverRelay(t *testing.T) {
	shielded, err := ConfigFromPolicy(sandboxpolicy.Policy{Shields: true}, policyOptions())
	require.NoError(t, err)
	assert.False(t, shielded.NoDNS, "a raised shield keeps public DNS available")

	ordinary, err := ConfigFromPolicy(sandboxpolicy.Policy{}, policyOptions())
	require.NoError(t, err)
	assert.False(t, ordinary.NoDNS)
}

func TestConfigFromPolicy_HostAddressesAreNeverPublicEgress(t *testing.T) {
	config, err := ConfigFromPolicy(sandboxpolicy.Policy{}, policyOptions("93.184.216.34/32"))
	require.NoError(t, err)

	decision := config.Classifier.Allow(netip.MustParseAddr("93.184.216.34"), sandboxpolicy.ProtocolTCP, 443)
	assert.False(t, decision.Allowed, "the daemon's own address is not egress")
	assert.Equal(t, "daemon host attached network", decision.Reason)

	assert.True(
		t,
		config.Classifier.Allow(netip.MustParseAddr("93.184.216.35"), sandboxpolicy.ProtocolTCP, 443).Allowed,
	)
}

func TestConfigFromPolicy_PublicAttachedSubnetIsNotPublicEgress(t *testing.T) {
	config, err := ConfigFromPolicy(sandboxpolicy.Policy{}, policyOptions("93.184.216.0/24"))
	require.NoError(t, err)
	assert.False(
		t,
		config.Classifier.Allow(netip.MustParseAddr("93.184.216.35"), sandboxpolicy.ProtocolTCP, 443).Allowed,
	)
	assert.True(
		t,
		config.Classifier.Allow(netip.MustParseAddr("93.184.217.35"), sandboxpolicy.ProtocolTCP, 443).Allowed,
	)
}

func TestConfigFromPolicy_ReportsHostEnumerationFailure(t *testing.T) {
	opts := policyOptions()
	opts.HostNetworks = func() ([]netip.Prefix, error) { return nil, assert.AnError }

	_, err := ConfigFromPolicy(sandboxpolicy.Policy{}, opts)
	require.ErrorIs(t, err, assert.AnError)
}

func TestPrefixOf_ReportsTheAttachedNetwork(t *testing.T) {
	network := &net.IPNet{IP: net.ParseIP("10.1.2.3"), Mask: net.CIDRMask(8, 32)}

	prefix, ok := prefixOf(network)
	require.True(t, ok)
	assert.Equal(t, "10.0.0.0/8", prefix.String())
}
