package logger

import (
	"fmt"

	"go.uber.org/zap/zapcore"
)

var _ zapcore.Core = (*sessionPrefixCore)(nil)

// sessionPrefixCore is a zapcore.Core wrapper that intercepts session_id and
// agent_id fields, removes them from output, and prepends a [root:agent] prefix
// to the log message.
type sessionPrefixCore struct {
	inner     zapcore.Core
	sessionID int64
	agentID   int64
	hasIDs    bool
}

func newSessionPrefixCore(inner zapcore.Core) zapcore.Core {
	return &sessionPrefixCore{inner: inner}
}

func (c *sessionPrefixCore) Enabled(lvl zapcore.Level) bool {
	return c.inner.Enabled(lvl)
}

func (c *sessionPrefixCore) With(fields []zapcore.Field) zapcore.Core {
	var (
		sessionID = c.sessionID
		agentID   = c.agentID
		hasIDs    = c.hasIDs
		kept      []zapcore.Field
	)

	for _, f := range fields {
		if f.Key == "session_id" && f.Type == zapcore.Int64Type {
			sessionID = f.Integer
			hasIDs = true

			continue
		}

		if f.Key == "agent_id" && f.Type == zapcore.Int64Type {
			agentID = f.Integer
			hasIDs = true

			continue
		}

		kept = append(kept, f)
	}

	return &sessionPrefixCore{
		inner:     c.inner.With(kept),
		sessionID: sessionID,
		agentID:   agentID,
		hasIDs:    hasIDs,
	}
}

func (c *sessionPrefixCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.inner.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}

	return ce
}

func (c *sessionPrefixCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	if c.hasIDs {
		entry.Message = c.prefix() + " " + entry.Message
	}

	// Also filter any session_id/agent_id from per-call fields.
	kept := filterSessionFields(fields)

	return c.inner.Write(entry, kept)
}

func (c *sessionPrefixCore) Sync() error {
	return c.inner.Sync()
}

func (c *sessionPrefixCore) prefix() string {
	if c.sessionID == c.agentID {
		return fmt.Sprintf("[%d]", c.sessionID)
	}

	return fmt.Sprintf("[%d:%d]", c.sessionID, c.agentID)
}

func filterSessionFields(fields []zapcore.Field) []zapcore.Field {
	var kept []zapcore.Field

	for _, f := range fields {
		if (f.Key == "session_id" || f.Key == "agent_id") && f.Type == zapcore.Int64Type {
			continue
		}

		kept = append(kept, f)
	}

	return kept
}
