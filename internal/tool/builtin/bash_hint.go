package builtin

import "strings"

// sandboxDenialMarkers are the write-denial errno texts the backends surface:
// EROFS from bubblewrap ro-binds, EPERM from Seatbelt file-write deny.
var sandboxDenialMarkers = []string{
	"read-only file system",
	"operation not permitted",
}

// sandboxHint returns a note explaining write confinement when a failed
// command's output looks like a sandbox write denial, so the model can
// self-diagnose instead of guessing; "" when unconfined or no marker matches.
func sandboxHint(output string, writableRoots []string) string {
	if len(writableRoots) == 0 {
		return ""
	}

	lower := strings.ToLower(output)

	found := false

	for _, marker := range sandboxDenialMarkers {
		if strings.Contains(lower, marker) {
			found = true
			break
		}
	}

	if !found {
		return ""
	}

	return "Note: bash commands run under a filesystem-write sandbox; writable roots: " +
		strings.Join(writableRoots, ", ") +
		". If the failed write is legitimate (e.g. a toolchain or package cache), " +
		"the operator can add the path to tools.bash.sandbox.writable_paths in the " +
		"coagent config (daemon restart required)."
}
