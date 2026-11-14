package configapply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

func TestConfigRead_SharesEditGrantAndReturnsCurrentDocumentVerbatim(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	document := "# operator formatting\nproviders: {}\n\nmodels: []\n"
	require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
	service := New(configops.New(path, "unused-test-secrets"))
	read := NewConfigRead(7, service)
	ctx := tool.WithActivationGrant(tool.WithCallID(t.Context(), "read-1"), tool.ActivationGrant{
		SessionID: 7, ToolID: tool.IDConfigEdit, Command: ConfigEditCommand,
	})
	result, err := read.Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, document, result.Output)
	document += "# newer document\n"
	require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
	result, err = read.Execute(ctx, json.RawMessage(`{}`))
	require.NoError(t, err)
	assert.Equal(t, document, result.Output)
	assert.True(t, read.ParallelSafe())
	assert.True(t, json.Valid(read.Parameters()))
	assert.Equal(t, tool.IDConfigRead, read.ID())
	_, declares := read.(tool.ActivationDeclarer)
	assert.False(t, declares)
	registry := tool.NewRegistry()
	registry.Register(NewConfigEdit(7, service))
	registry.Register(read)
	index, err := tool.ActivationIndex(registry)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{ConfigEditCommand: tool.IDConfigEdit}, index)
}

func TestConfigRead_RefusesMissingForeignAndBoundGrantsBeforeReading(t *testing.T) {
	t.Parallel()
	read := NewConfigRead(7, New(configops.New(filepath.Join(t.TempDir(), "missing.yaml"), "unused")))
	_, err := read.Execute(t.Context(), json.RawMessage(`{}`))
	require.EqualError(t, err, noConfigActivationMessage)
	for _, grant := range []tool.ActivationGrant{
		{SessionID: 8, ToolID: tool.IDConfigEdit, Command: ConfigEditCommand},
		{SessionID: 7, ToolID: tool.IDConfigRead, Command: ConfigEditCommand},
		{SessionID: 7, ToolID: tool.IDConfigEdit, Command: "/configx"},
		{SessionID: 0, ToolID: tool.IDConfigEdit, Command: ConfigEditCommand},
	} {
		ctx := tool.WithActivationGrant(tool.WithCallID(t.Context(), "read-1"), grant)
		_, err := read.Execute(ctx, json.RawMessage(`{}`))
		require.EqualError(t, err, noConfigActivationMessage)
	}
}

func TestConfigRead_ReportsUnavailableAndMissingConfiguration(t *testing.T) {
	t.Parallel()
	ctx := tool.WithActivationGrant(tool.WithCallID(t.Context(), "read-1"), tool.ActivationGrant{
		SessionID: 7, ToolID: tool.IDConfigEdit, Command: ConfigEditCommand,
	})
	_, err := NewConfigRead(7, nil).Execute(ctx, json.RawMessage(`{}`))
	require.ErrorContains(t, err, "cannot read its configuration")
	read := NewConfigRead(7, New(configops.New(filepath.Join(t.TempDir(), "missing.yaml"), "unused")))
	_, err = read.Execute(ctx, json.RawMessage(`{}`))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = read.Execute(ctx, json.RawMessage(`{`))
	require.ErrorContains(t, err, "parse config read parameters")
}
