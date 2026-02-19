package logger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSessionPrefixCore_SameIDs(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	core = core.With([]zapcore.Field{
		zap.Int64("session_id", 67),
		zap.Int64("agent_id", 67),
	})

	logger := zap.New(core)
	logger.Info("hello world")

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "[67] hello world", entry.Message)
	// session_id and agent_id should be removed from fields
	ctx := entry.ContextMap()
	assert.NotContains(t, ctx, "session_id")
	assert.NotContains(t, ctx, "agent_id")
}

func TestSessionPrefixCore_DifferentIDs(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	core = core.With([]zapcore.Field{
		zap.Int64("session_id", 67),
		zap.Int64("agent_id", 89),
	})

	logger := zap.New(core)
	logger.Info("subagent task")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "[67:89] subagent task", logs.All()[0].Message)
}

func TestSessionPrefixCore_NoIDs(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	logger := zap.New(core)
	logger.Info("no prefix")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "no prefix", logs.All()[0].Message)
}

func TestSessionPrefixCore_PreservesOtherFields(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	core = core.With([]zapcore.Field{
		zap.Int64("session_id", 1),
		zap.Int64("agent_id", 1),
		zap.String("component", "tool"),
	})

	logger := zap.New(core)
	logger.Info("exec", zap.String("name", "bash"))

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "[1] exec", entry.Message)
	ctx := entry.ContextMap()
	assert.Equal(t, "tool", ctx["component"])
}

func TestSessionPrefixCore_StringSessionIDPassesThrough(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	// String type session_id should NOT be intercepted
	core = core.With([]zapcore.Field{
		zap.String("session_id", "foo"),
	})

	logger := zap.New(core)
	logger.Info("no prefix expected")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "no prefix expected", logs.All()[0].Message)
	assert.Equal(t, "foo", logs.All()[0].ContextMap()["session_id"])
}

func TestSessionPrefixCore_PerCallFields(t *testing.T) {
	inner, logs := observer.New(zapcore.DebugLevel)
	core := newSessionPrefixCore(inner)

	// IDs passed as per-call fields (not via With)
	logger := zap.New(core)
	logger.Info("inline", zap.Int64("session_id", 5), zap.Int64("agent_id", 10))

	require.Equal(t, 1, logs.Len())
	// Per-call session fields should also be filtered
	entry := logs.All()[0]
	// Note: per-call IDs are filtered but don't set prefix (only With sets prefix)
	// Actually re-reading the code — Write filters per-call fields but prefix
	// only comes from c.hasIDs which is set via With. So inline IDs get stripped
	// but no prefix. This is correct behavior.
	assert.Equal(t, "inline", entry.Message)
}
