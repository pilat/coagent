package builtin

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebFetchBlockedAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr netip.Addr
	}{
		{"ipv4 metadata", netip.MustParseAddr("169.254.169.254")},
		{"ipv4 link-local", netip.MustParseAddr("169.254.1.1")},
		{"ipv4 metadata mapped into ipv6", netip.MustParseAddr("::ffff:169.254.169.254")},
		{"ipv6 link-local", netip.MustParseAddr("fe80::1")},
		{"ipv6 link-local with literal zone", netip.MustParseAddr("fe80::1%en0")},
		{"ipv6 link-local with attached zone", netip.MustParseAddr("fe80::1").WithZone("eth0")},
		{"ipv6 metadata endpoint", netip.MustParseAddr("fd00:ec2::254")},
		{"nat64-wrapped ipv4 metadata", netip.MustParseAddr("64:ff9b::a9fe:a9fe")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, isBlockedFetchAddr(normalizeFetchAddr(tt.addr)))
		})
	}
}

func TestWebFetchAllowedAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr netip.Addr
	}{
		{"ipv4 loopback", netip.MustParseAddr("127.0.0.1")},
		{"ipv6 loopback", netip.MustParseAddr("::1")},
		{"private 10/8", netip.MustParseAddr("10.0.0.1")},
		{"private 192.168/16", netip.MustParseAddr("192.168.1.10")},
		{"private 172.16/12", netip.MustParseAddr("172.16.0.1")},
		{"cgnat", netip.MustParseAddr("100.64.0.1")},
		{"ula that is not the metadata endpoint", netip.MustParseAddr("fd00::1")},
		{"public ipv4", netip.MustParseAddr("93.184.216.34")},
		{"nat64-wrapped public ipv4", netip.MustParseAddr("64:ff9b::5db8:d822")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, isBlockedFetchAddr(normalizeFetchAddr(tt.addr)))
		})
	}
}

func TestWebFetchNormalizeAddr(t *testing.T) {
	tests := []struct {
		name string
		addr netip.Addr
		want netip.Addr
	}{
		{"strips zone", netip.MustParseAddr("fe80::1%en0"), netip.MustParseAddr("fe80::1")},
		{"unwraps nat64", netip.MustParseAddr("64:ff9b::5db8:d822"), netip.MustParseAddr("93.184.216.34")},
		{"unmaps 4-in-6", netip.MustParseAddr("::ffff:127.0.0.1"), netip.MustParseAddr("127.0.0.1")},
		{"leaves ula alone", netip.MustParseAddr("fd00::1"), netip.MustParseAddr("fd00::1")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeFetchAddr(tt.addr))
		})
	}
}

func TestWebFetchDialControl(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		wantErrText string
	}{
		{"ipv4 metadata", "169.254.169.254:80", "169.254.169.254"},
		{"zoned ipv6 link-local", "[fe80::1%en0]:80", "fe80::1%en0"},
		{"loopback", "127.0.0.1:8080", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := webFetchDialControl(context.Background(), "tcp4", tt.address, nil)
			if tt.wantErrText == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrText)
		})
	}
}

func TestWebFetchRestrictedTransportIgnoresProxy(t *testing.T) {
	transport := newRestrictedTransport()

	assert.Nil(t, transport.Proxy)
	assert.NotNil(t, transport.DialContext)
}
