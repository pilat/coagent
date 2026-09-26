package sandboxpolicy

import (
	"path/filepath"
	"sort"
	"strings"
)

const (
	// TempPath and VarTempPath are the paths workloads see for their private
	// temporary storage; the backing directories are project-owned.
	TempPath    = "/tmp"
	VarTempPath = "/var/tmp"

	// legacyProfile labels grants translated from the deprecated
	// sandbox.writable_paths input.
	legacyProfile = "legacy"
)

// mergeGrants deduplicates grants by target, promotes an exact mount to
// read-write, and stops advertising a nested read-only rule beneath a granted
// read-write ancestor.
func mergeGrants(grants []Grant) []Grant {
	merged := make(map[string]Grant, len(grants))

	for _, grant := range grants {
		existing, ok := merged[grant.Target]
		if !ok {
			merged[grant.Target] = grant

			continue
		}

		merged[grant.Target] = combineGrant(existing, grant)
	}

	ordered := make([]Grant, 0, len(merged))
	for _, grant := range merged {
		ordered = append(ordered, grant)
	}

	ordered = dropNestedReadOnly(ordered)
	sortGrants(ordered)

	return ordered
}

// combineGrant keeps the wider mode and the more specific label of two grants
// addressing the same target.
func combineGrant(existing, incoming Grant) Grant {
	winner := existing

	if existing.Mode == ModeReadOnly && incoming.Mode == ModeReadWrite {
		winner = incoming
	}

	if winner.Profile == "" && incoming.Profile != "" {
		winner.Profile = incoming.Profile
	}

	if winner.Level == "" {
		winner.Level = incoming.Level
	}

	if !incoming.Present {
		winner.Present = false
	}

	return winner
}

func dropNestedReadOnly(grants []Grant) []Grant {
	kept := make([]Grant, 0, len(grants))

	for _, grant := range grants {
		if grant.Mode == ModeReadOnly && coveredByWritableAncestor(grant, grants) {
			continue
		}

		kept = append(kept, grant)
	}

	return kept
}

// coveredByWritableAncestor reports whether a read-write grant already covers
// the same object hierarchy, which makes a nested read-only rule meaningless.
func coveredByWritableAncestor(grant Grant, grants []Grant) bool {
	for _, other := range grants {
		if other.Mode != ModeReadWrite || other.Target == grant.Target {
			continue
		}

		if !pathWithin(other.Target, grant.Target) {
			continue
		}

		if pathWithin(other.Source, grant.Source) {
			return true
		}
	}

	return false
}

func sortGrants(grants []Grant) {
	sort.Slice(grants, func(i, j int) bool {
		left, right := pathDepth(grants[i].Target), pathDepth(grants[j].Target)
		if left != right {
			return left < right
		}

		return grants[i].Target < grants[j].Target
	})
}

// mergeSockets keeps one grant per canonical socket path, with a basic entry
// winning over an escalated one and presence merged conservatively.
func mergeSockets(sockets []SocketGrant) []SocketGrant {
	merged := make(map[string]SocketGrant, len(sockets))

	for _, socket := range sockets {
		existing, ok := merged[socket.Path]
		if !ok {
			merged[socket.Path] = socket

			continue
		}

		if existing.Level == LevelEscalated && socket.Level == LevelBasic {
			socket.Present = socket.Present || existing.Present
			merged[socket.Path] = socket

			continue
		}

		if !socket.Present {
			existing.Present = false
			merged[socket.Path] = existing
		}
	}

	ordered := make([]SocketGrant, 0, len(merged))
	for _, socket := range merged {
		ordered = append(ordered, socket)
	}

	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	return ordered
}

// mergeNetwork keeps one rule per address/protocol/port set, with a basic entry
// winning over an escalated one.
func mergeNetwork(entries []Network) []Network {
	merged := make(map[string]Network, len(entries))

	for _, entry := range entries {
		key := networkKey(entry)

		existing, ok := merged[key]
		if ok && (existing.Type != LevelEscalated || entry.Type != LevelBasic) {
			continue
		}

		merged[key] = entry
	}

	ordered := make([]Network, 0, len(merged))
	for _, entry := range merged {
		ordered = append(ordered, entry)
	}

	sort.Slice(ordered, func(i, j int) bool { return networkKey(ordered[i]) < networkKey(ordered[j]) })

	return ordered
}

// pathWithin reports whether path is root itself or lies beneath it.
func pathWithin(root, path string) bool {
	if root == path || root == string(filepath.Separator) {
		return true
	}

	if !strings.HasPrefix(path, root) {
		return false
	}

	return strings.HasPrefix(path[len(root):], string(filepath.Separator))
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}
