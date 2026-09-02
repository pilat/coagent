package builtin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

// TestCoreRegistry_ParallelSafePolicies pins the concurrency declarations of
// the model-facing core stack: an accidental opt-in or opt-out must fail here.
func TestCoreRegistry_ParallelSafePolicies(t *testing.T) {
	mutator, err := newFileMutator(false, nil)
	require.NoError(t, err)

	reg := tool.NewRegistry()
	registerCoreTools(reg, t.TempDir(), nil, nil, nil, nil, nil, mutator)

	want := map[string]bool{
		"read":        true,
		"write":       false,
		"edit":        false,
		"apply_patch": false,
		"ls":          true,
		"glob":        true,
		"grep":        true,
		"bash":        false,
		"webfetch":    true,
		"skill":       false,
		"todoread":    true,
		"todowrite":   false,
		"batch":       false,
		"lsp":         false,
	}

	got := make(map[string]bool, len(want))
	for _, tl := range reg.List() {
		got[tl.ID()] = tl.ParallelSafe()
	}

	assert.Equal(t, want, got)
}
