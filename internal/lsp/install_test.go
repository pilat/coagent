package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestCoagentBinUnresolvableHome(t *testing.T) {
	restore := coagenthome.Override("")
	defer restore()

	assert.Empty(t, coagentBin())
	require.Error(t, ensureCoagentBin())
}

func TestEnsureCoagentBinCreatesDir(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	defer restore()

	require.NoError(t, ensureCoagentBin())
	assert.DirExists(t, coagentBin())
}
