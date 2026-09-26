package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"strings"
)

// Level is a permission's activation class. Basic entries apply whenever the
// sandbox is enabled; escalated entries require an explicit operator grant.
type Level string

const (
	LevelBasic     Level = "basic"
	LevelEscalated Level = "escalated"
)

// Mode is a mount entry's access mode.
type Mode string

const (
	ModeReadOnly  Mode = "ro"
	ModeReadWrite Mode = "rw"
)

// Protocol is a network entry's transport protocol.
type Protocol string

const (
	ProtocolTCP  Protocol = "tcp"
	ProtocolUDP  Protocol = "udp"
	ProtocolICMP Protocol = "icmp"
)

// ObjectKind is the filesystem kind a mount entry expects at its anchor.
type ObjectKind string

const (
	KindDir  ObjectKind = "dir"
	KindFile ObjectKind = "file"
)

// HostLoopback is the reserved network target that maps to daemon-host
// loopback. Ordinary loopback addresses inside the sandbox never mean the host.
const HostLoopback = "host-loopback"

type (
	// Mount grants one host path to workloads, read-only or read-write.
	Mount struct {
		Path string     `json:"path"           yaml:"path"`
		Mode Mode       `json:"mode"           yaml:"mode"`
		Type Level      `json:"type"           yaml:"type"`
		Kind ObjectKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	}

	// Socket grants one exact pathname Unix socket outside mounted directories.
	// A socket entry has no mode: socket permissions are the socket's own.
	Socket struct {
		Path string `json:"path" yaml:"path"`
		Type Level  `json:"type" yaml:"type"`
	}

	// Network grants reachability to a non-public destination. Public egress
	// needs no entry.
	Network struct {
		Address  string   `json:"address"  yaml:"address"`
		Protocol Protocol `json:"protocol" yaml:"protocol"`
		Ports    []int    `json:"ports"    yaml:"ports"`
		Type     Level    `json:"type"     yaml:"type"`
	}

	// Profile groups the permissions one developer tool needs.
	Profile struct {
		Mounts  []Mount   `json:"mounts,omitempty"  yaml:"mounts,omitempty"`
		Sockets []Socket  `json:"sockets,omitempty" yaml:"sockets,omitempty"`
		Network []Network `json:"network,omitempty" yaml:"network,omitempty"`
	}

	// ProjectOverride carries the escalation a single project adds.
	ProjectOverride struct {
		Escalated []string `json:"escalated,omitempty" yaml:"escalated,omitempty"`
	}

	// Section is the strict sandbox configuration section. The schema lives here
	// because operator-defined profiles decode into the same types.
	Section struct {
		Enabled       bool                       `json:"enabled"                  yaml:"enabled"`
		Escalated     []string                   `json:"escalated,omitempty"      yaml:"escalated,omitempty"`
		Projects      map[string]ProjectOverride `json:"projects,omitempty"       yaml:"projects,omitempty"`
		Profiles      map[string]Profile         `json:"profiles,omitempty"       yaml:"profiles,omitempty"`
		WritablePaths []string                   `json:"writable_paths,omitempty" yaml:"writable_paths,omitempty"`
	}
)

func (l Level) valid() bool {
	return l == LevelBasic || l == LevelEscalated
}

func (m Mode) valid() bool {
	return m == ModeReadOnly || m == ModeReadWrite
}

func (p Protocol) valid() bool {
	return p == ProtocolTCP || p == ProtocolUDP || p == ProtocolICMP
}

func (k ObjectKind) valid() bool {
	return k == "" || k == KindDir || k == KindFile
}

func (k ObjectKind) resolved() ObjectKind {
	if k == KindFile {
		return KindFile
	}

	return KindDir
}

// validateNetworkEntry checks one network grant's address, protocol and ports.
func validateNetworkEntry(entry Network) error {
	if !entry.Protocol.valid() {
		return fmt.Errorf(
			"network entry %q has invalid protocol %q, must be one of: tcp, udp, icmp",
			entry.Address,
			entry.Protocol,
		)
	}

	if err := validateNetworkAddress(entry.Address); err != nil {
		return err
	}

	if entry.Protocol == ProtocolICMP {
		if len(entry.Ports) != 0 {
			return fmt.Errorf("network entry %q: ICMP grants must not specify ports", entry.Address)
		}

		return nil
	}

	if len(entry.Ports) == 0 {
		return fmt.Errorf("network entry %q requires a non-empty \"ports\" list", entry.Address)
	}

	for _, port := range entry.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("network entry %q has invalid port %d, must be 1-65535", entry.Address, port)
		}
	}

	return nil
}

// validateNetworkAddress accepts an IP, a CIDR prefix, or the reserved
// host-loopback target. Domain names are not a filtering claim and are refused.
func validateNetworkAddress(address string) error {
	if address == HostLoopback {
		return nil
	}

	if strings.ContainsAny(address, " \t") {
		return fmt.Errorf("network address %q must not contain whitespace", address)
	}

	if strings.Contains(address, "/") {
		if _, err := netip.ParsePrefix(address); err != nil {
			return fmt.Errorf("network address %q is not a valid CIDR prefix: %w", address, err)
		}

		return nil
	}

	if _, err := netip.ParseAddr(address); err != nil {
		return fmt.Errorf("network address %q is neither an IP, a CIDR prefix, nor %q", address, HostLoopback)
	}

	return nil
}
