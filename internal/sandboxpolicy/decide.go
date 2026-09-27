package sandboxpolicy

import (
	"path/filepath"
)

// AllowsRead reports whether the policy admits reading a host path.
func (p Policy) AllowsRead(path string) bool {
	action, mode, _, ok := decide(p.Entries, path)

	return ok && action == ActionAllow && (mode == ModeReadOnly || mode == ModeReadWrite)
}

// AllowsWrite reports whether the policy admits writing a host path.
func (p Policy) AllowsWrite(path string) bool {
	action, mode, _, ok := decide(p.Entries, path)

	return ok && action == ActionAllow && mode == ModeReadWrite
}

// EffectiveEntries excludes rules wholly superseded by a later ancestor rule.
func (p Policy) EffectiveEntries() []Entry {
	entries := make([]Entry, 0, len(p.Entries))
	for i, entry := range p.Entries {
		_, _, winner, _ := decide(p.Entries, entry.Path)
		if winner == i {
			entries = append(entries, entry)
		}
	}

	return entries
}

// WritableRoots reports the surviving read-write grant paths.
func (p Policy) WritableRoots() []string {
	roots := make([]string, 0, len(p.Entries))

	for _, entry := range p.EffectiveEntries() {
		if entry.Action == ActionAllow && entry.Mode == ModeReadWrite {
			roots = append(roots, entry.Path)
		}
	}

	return roots
}

// decide walks entries in order and returns the last one that matches path,
// along with its index, so a caller can name the entry that decided.
func decide(entries []Entry, path string) (Action, Mode, int, bool) {
	if !filepath.IsAbs(path) {
		return "", "", -1, false
	}

	clean := filepath.Clean(path)

	action, mode, winner := Action(""), Mode(""), -1
	matched := false

	for i, entry := range entries {
		if !entryMatches(entry, clean) {
			continue
		}

		action, mode, winner, matched = entry.Action, entry.Mode, i, true
	}

	return action, mode, winner, matched
}

func entryMatches(entry Entry, path string) bool {
	return pathWithin(entry.Path, path)
}
