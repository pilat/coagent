package sandboxpolicy

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateNetworkAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{name: "ipv4 address", address: "10.1.2.3"},
		{name: "ipv6 address", address: "2001:db8::1"},
		{name: "ipv4 prefix", address: "10.0.0.0/8"},
		{name: "ipv6 prefix", address: "fd00::/8"},
		{name: "host loopback", address: HostLoopback},
		{name: "domain name", address: "example.com", wantErr: true},
		{name: "prefix out of range", address: "10.0.0.0/33", wantErr: true},
		{name: "trailing whitespace", address: "10.0.0.1 ", wantErr: true},
		{name: "empty", address: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNetworkAddress(tt.address)

			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			assert.NoError(t, err)
		})
	}
}

func TestValidateNetworkEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   Network
		wantErr bool
	}{
		{
			name:  "tcp grant",
			entry: Network{Address: "10.0.0.5", Protocol: ProtocolTCP, Ports: []int{5432}, Type: LevelBasic},
		},
		{
			name:  "udp grant",
			entry: Network{Address: HostLoopback, Protocol: ProtocolUDP, Ports: []int{53}, Type: LevelEscalated},
		},
		{
			name:    "unknown protocol",
			entry:   Network{Address: "10.0.0.5", Protocol: "sctp", Ports: []int{1}, Type: LevelBasic},
			wantErr: true,
		},
		{
			name:  "icmp IPv4 grant",
			entry: Network{Address: "10.0.0.5", Protocol: ProtocolICMP, Type: LevelEscalated},
		},
		{
			name:  "icmp IPv6 grant",
			entry: Network{Address: "fd00::/8", Protocol: ProtocolICMP, Type: LevelEscalated},
		},
		{
			name:    "icmp does not have ports",
			entry:   Network{Address: "10.0.0.5", Protocol: ProtocolICMP, Ports: []int{8}, Type: LevelEscalated},
			wantErr: true,
		},
		{
			name:    "no ports",
			entry:   Network{Address: "10.0.0.5", Protocol: ProtocolTCP, Type: LevelBasic},
			wantErr: true,
		},
		{
			name:    "port out of range",
			entry:   Network{Address: "10.0.0.5", Protocol: ProtocolTCP, Ports: []int{70000}, Type: LevelBasic},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNetworkEntry(tt.entry)

			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			assert.NoError(t, err)
		})
	}
}

func TestNormalizeAnchor(t *testing.T) {
	home := testHome(t)

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "absolute path", path: "/srv/data", want: "/srv/data"},
		{name: "home expansion", path: "~/cache", want: filepath.Join(home, "cache")},
		{name: "relative path", path: "cache", wantErr: true},
		{name: "unsupported expansion", path: "~other/cache", wantErr: true},
		{name: "empty", path: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeAnchor(tt.path)

			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidateGrantPath_BroadRoots(t *testing.T) {
	home := testHome(t)

	controlDir, err := coagenthomeDir(t)
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		socket  bool
		wantErr bool
	}{
		{name: "filesystem root", path: "/", wantErr: true},
		{name: "home root", path: home, wantErr: true},
		{name: "whole tmp", path: "/tmp", wantErr: true},
		{name: "whole run", path: "/run", wantErr: true},
		{name: "proc", path: "/proc", wantErr: true},
		{name: "sys", path: "/sys", wantErr: true},
		{name: "coagent control dir", path: controlDir, wantErr: true},
		{name: "coagent control child", path: filepath.Join(controlDir, "cache"), wantErr: true},
		{name: "socket under run", path: filepath.Join("/run", "user", "1000", "agent.sock"), socket: true},
		{name: "mount under run", path: filepath.Join("/run", "user", "1000")},
		{name: "home child", path: filepath.Join(home, ".cache")},
		{name: "tmp child", path: filepath.Join("/tmp", "project")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGrantPath(tt.path, tt.socket)

			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			assert.NoError(t, err)
		})
	}
}
