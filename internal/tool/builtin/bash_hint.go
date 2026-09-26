package builtin

import (
	"strings"

	"github.com/pilat/coagent/internal/safefile"
)

// sandboxDenialMarkers are the write-denial errno texts the native sandbox
// surfaces: EROFS from read-only binds, EPERM from write-deny rules.
var sandboxDenialMarkers = []string{
	"read-only file system",
	"operation not permitted",
}

// sandboxHint returns a note explaining confinement when a failed command's
// output looks like a sandbox write denial, so the model can self-diagnose
// instead of guessing; "" when the sandbox is disabled or no marker matches.
func sandboxHint(output string, writableRoots []string, project string) string {
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

	return "Note: " + safefile.ShieldDeniedMessage + " Project boundary: " + project +
		"; writable roots: " + strings.Join(writableRoots, ", ") +
		". If the failed write is legitimate (e.g. a toolchain or package cache), " +
		"the operator can grant that path with a sandbox profile in the coagent config " +
		"(sandbox.profiles; daemon restart required)."
}
