package configtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

type fakeDocumentStager struct {
	staged  *configops.Staged
	verdict configops.Verdict
	called  bool
}

func (f *fakeDocumentStager) StageDocument(candidate []byte) (*configops.Staged, configops.Verdict) {
	f.called = true

	if len(bytes.TrimSpace(candidate)) == 0 {
		return nil, configops.Reject("", errors.New("empty configuration document"))
	}

	return f.staged, f.verdict
}

func newConfigEditFixture() (*configEditTool, *fakeDocumentStager, *bool) {
	stager := &fakeDocumentStager{
		staged:  &configops.Staged{Data: []byte("providers: {}"), Hash: "abc", Summary: "replace"},
		verdict: configops.OK(),
	}
	applied := false

	stage := func(callID, toolName string, staged *configops.Staged) bool {
		applied = true
		return true
	}

	return &configEditTool{stager: stager, stage: stage}, stager, &applied
}

func TestConfigEditTool_DeclaresExactCommandAndDeterministicSchema(t *testing.T) {
	t.Parallel()

	tl, _, _ := newConfigEditFixture()

	declarer, ok := tool.Tool(tl).(tool.ActivationDeclarer)
	require.True(t, ok)
	assert.Equal(t, []string{"/config"}, declarer.ActivationCommands())

	assert.Equal(t, tool.IDConfigEdit, tl.ID())
	first := tl.Parameters()
	second := tl.Parameters()
	assert.Equal(t, string(first), string(second), "schema must be deterministic")
	assert.NotEmpty(t, tl.Description())
}

func TestConfigEditTool_RequiresCurrentConfigGrant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tl, stager, applied := newConfigEditFixture()

	params := mustJSON(t, map[string]any{"document": "providers: {}"})

	_, err := tl.Execute(ctx, params)
	require.EqualError(t, err, noConfigActivationMessage)
	assert.False(t, stager.called)
	assert.False(t, *applied, "no apply starts without a grant")

	grant := tool.ActivationGrant{SessionID: 7, InputID: 3, ToolID: tool.IDConfigEdit, Command: "/config"}
	authoredCtx := func() context.Context {
		return tool.WithActivationGrant(tool.WithCallID(ctx, "call-1"), grant)
	}

	result, err := tl.Execute(authoredCtx(), params)
	require.Nil(t, result)
	require.ErrorIs(t, err, tool.ErrSuspend)
	assert.True(t, stager.called)
	assert.True(t, *applied)
}

func TestConfigEditTool_RejectsForeignGrants(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tl, stager, applied := newConfigEditFixture()
	params := mustJSON(t, map[string]any{"document": "providers: {}"})

	refused := []tool.ActivationGrant{
		{SessionID: 1, InputID: 3, ToolID: "set_budget", Command: "/budget"},
		{SessionID: 1, InputID: 3, ToolID: tool.IDConfigEdit, Command: "/configx"},
		{SessionID: 1, InputID: 3, ToolID: tool.IDConfigEdit, Command: "/config", ToolCallID: "other-call"},
	}

	for _, grant := range refused {
		authored := tool.WithActivationGrant(tool.WithCallID(ctx, "call-1"), grant)
		_, err := tl.Execute(authored, params)
		require.EqualError(t, err, noConfigActivationMessage, "grant %+v must not authorize", grant)
	}

	assert.False(t, stager.called)
	assert.False(t, *applied)
}

func TestConfigEditTool_InvalidCandidateReturnsOrdinaryError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tl, stager, applied := newConfigEditFixture()
	stager.staged = nil
	stager.verdict = configops.Reject("", errors.New("parsing config file: bad yaml"))

	authored := tool.WithActivationGrant(tool.WithCallID(ctx, "call-1"),
		tool.ActivationGrant{SessionID: 1, InputID: 3, ToolID: tool.IDConfigEdit, Command: "/config"})

	_, err := tl.Execute(authored, mustJSON(t, map[string]any{"document": "bad: [yaml"}))
	require.EqualError(t, err, "parsing config file: bad yaml")
	assert.True(t, stager.called)
	assert.False(t, *applied, "a rejected candidate never reaches the apply callback")
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return encoded
}

func TestConfigEditTool_MissingDocumentParameter(t *testing.T) {
	t.Parallel()

	tl, stager, _ := newConfigEditFixture()

	authored := tool.WithActivationGrant(tool.WithCallID(context.Background(), "call-1"),
		tool.ActivationGrant{SessionID: 1, InputID: 3, ToolID: tool.IDConfigEdit, Command: "/config"})

	_, err := tl.Execute(authored, mustJSON(t, map[string]any{}))
	require.Error(t, err)
	assert.False(t, stager.called)
}
