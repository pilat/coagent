package sessionbuild

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestLoadHooksWithoutSettingsReturnsNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	stack, err := builtin.BuildStack(t.Context(), builtin.StackConfig{WorkDir: workDir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })
	in := BuildInput{Record: &sessionstore.SessionRecord{ID: 42}, WorkDir: workDir}
	assert.Nil(t, loadHooks(t.Context(), in, stack))
}
