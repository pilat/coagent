package sandboxpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// digest is the identity of one effective policy generation. It covers every
// authorized anchor — including declared ones whose object is currently absent
// — plus permissions, sockets, network rules, shield state, catalog version and
// project identity. Object presence and inode are deliberately excluded.
func (p Policy) digest() string {
	parts := make([]string, 0, 4+len(p.Grants)+len(p.Sockets)+len(p.Network))
	parts = append(parts,
		"catalog:"+p.CatalogVersion,
		"project:"+p.ProjectRoot,
		fmt.Sprintf("project_id:%d", p.ProjectID),
		fmt.Sprintf("shields:%t", p.Shields),
	)

	for _, grant := range p.Grants {
		parts = append(parts, fmt.Sprintf(
			"grant:%s:%s:%s:%s:%s:%s", grant.Profile, grant.Level, grant.Source, grant.Target, grant.Mode, grant.Kind,
		))
	}

	for _, socket := range p.Sockets {
		parts = append(parts, fmt.Sprintf("socket:%s:%s:%s", socket.Profile, socket.Level, socket.Path))
	}

	for _, entry := range p.Network {
		parts = append(parts, fmt.Sprintf(
			"network:%s:%s:%v:%s", entry.Address, entry.Protocol, entry.Ports, entry.Type,
		))
	}

	hash := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	return hex.EncodeToString(hash[:])
}

// WritableRoots reports the granted read-write roots, deepest first.
func (p Policy) WritableRoots() []string {
	roots := make([]string, 0, len(p.Grants))

	for _, grant := range p.Grants {
		if grant.Mode == ModeReadWrite {
			roots = append(roots, grant.Target)
		}
	}

	sort.Strings(roots)

	return roots
}

// AllowsRead reports whether the policy grants read access to a path.
func (p Policy) AllowsRead(path string) bool {
	for _, grant := range p.Grants {
		if pathWithin(grant.Target, path) {
			return true
		}
	}

	return false
}

// AllowsWrite reports whether the policy grants write access to a path.
func (p Policy) AllowsWrite(path string) bool {
	for _, grant := range p.Grants {
		if grant.Mode == ModeReadWrite && pathWithin(grant.Target, path) {
			return true
		}
	}

	return false
}
