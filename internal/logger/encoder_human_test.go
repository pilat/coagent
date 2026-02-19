package logger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

func TestHumanEncoder_BasicEntry(t *testing.T) {
	enc := newHumanEncoder()

	entry := zapcore.Entry{
		Level:      zapcore.InfoLevel,
		Time:       time.Date(2026, 4, 4, 15, 30, 45, 0, time.UTC),
		LoggerName: "session",
		Message:    "started",
	}

	buf, err := enc.EncodeEntry(entry, nil)
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	// Should contain the time, level, component, and message
	assert.Contains(t, out, "15:30:45")
	assert.Contains(t, out, "INFO")
	assert.Contains(t, out, "session")
	assert.Contains(t, out, "started")
}

func TestHumanEncoder_WithFields(t *testing.T) {
	enc := newHumanEncoder()

	entry := zapcore.Entry{
		Level:      zapcore.WarnLevel,
		Time:       time.Date(2026, 4, 4, 10, 0, 0, 0, time.UTC),
		LoggerName: "tool",
		Message:    "slow execution",
	}

	fields := []zapcore.Field{
		{Key: "duration", Type: zapcore.StringType, String: "5s"},
	}

	buf, err := enc.EncodeEntry(entry, fields)
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	assert.Contains(t, out, "WARN")
	assert.Contains(t, out, "tool")
	assert.Contains(t, out, "slow execution")
	assert.Contains(t, out, "duration=5s")
}

func TestHumanEncoder_NoLoggerName(t *testing.T) {
	enc := newHumanEncoder()

	entry := zapcore.Entry{
		Level:   zapcore.DebugLevel,
		Time:    time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		Message: "no component",
	}

	buf, err := enc.EncodeEntry(entry, nil)
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	assert.Contains(t, out, "DEBUG")
	assert.Contains(t, out, "no component")
	// Should NOT have the component column padding
}

func TestHumanEncoder_Clone(t *testing.T) {
	enc := newHumanEncoder().(*humanEncoder)
	enc.addField("existing", "value")

	clone := enc.Clone().(*humanEncoder)
	clone.addField("new", "other")

	// Original should not be affected
	assert.Len(t, enc.fields, 1)
	assert.Len(t, clone.fields, 2)
}

func TestHumanEncoder_ErrorLevel(t *testing.T) {
	enc := newHumanEncoder()

	entry := zapcore.Entry{
		Level:   zapcore.ErrorLevel,
		Time:    time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		Message: "something broke",
	}

	buf, err := enc.EncodeEntry(entry, nil)
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	assert.Contains(t, out, "ERROR")
	assert.Contains(t, out, "something broke")
}

func TestHumanEncoder_FieldTypes(t *testing.T) {
	enc := newHumanEncoder()

	entry := zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Date(2026, 4, 4, 12, 0, 0, 0, time.UTC),
		Message: "types",
	}

	fields := []zapcore.Field{
		zap.Float64("cost", 3.14),
		zap.Duration("elapsed", 5*time.Second),
		zap.Int64("count", 42),
		zap.Bool("ok", true),
	}

	buf, err := enc.EncodeEntry(entry, fields)
	require.NoError(t, err)
	defer buf.Free()

	out := buf.String()
	assert.Contains(t, out, "cost=3.14")
	assert.Contains(t, out, "elapsed=5s")
	assert.Contains(t, out, "count=42")
	assert.Contains(t, out, "ok=true")
}

func TestHumanEncoder_ReturnsBuffer(t *testing.T) {
	enc := newHumanEncoder()
	entry := zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Now(),
		Message: "test",
	}
	buf, err := enc.EncodeEntry(entry, nil)
	require.NoError(t, err)
	require.IsType(t, &buffer.Buffer{}, buf)
	buf.Free()
}
