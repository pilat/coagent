package logger

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap/zapcore"
)

const redactedPlaceholder = "[REDACTED]"

// redactedValues is sorted longest-first so an overlapping shorter secret
// cannot leave a residue of a longer one.
var redactedValues atomic.Pointer[[]string]

// redactMu serializes writers; readers stay lock-free on the atomic pointer.
var redactMu sync.Mutex

// redactingCore is a zapcore.Core wrapper that scrubs registered secrets from
// messages and string/byte-string/error fields (error fields flatten on hit).
type redactingCore struct {
	inner zapcore.Core
}

var _ zapcore.Core = (*redactingCore)(nil)

// SetRedactedValues installs the secret strings scrubbed from every log entry.
// Empty values are dropped. Safe to call at any time, from any goroutine.
func SetRedactedValues(values []string) {
	redactMu.Lock()
	defer redactMu.Unlock()

	store(values)
}

// AddRedactedValues registers more secrets without dropping the ones already
// registered: a credential written while the daemon runs must be scrubbed from
// that moment on, and the boot-time set is still live.
func AddRedactedValues(values ...string) {
	redactMu.Lock()
	defer redactMu.Unlock()

	store(append(loadRedactedValues(), values...))
}

func newRedactingCore(inner zapcore.Core) zapcore.Core {
	return &redactingCore{inner: inner}
}

// Redact scrubs registered secrets from s, for output that bypasses zap.
func Redact(s string) string {
	return redact(s, loadRedactedValues())
}

func (c *redactingCore) Enabled(lvl zapcore.Level) bool {
	return c.inner.Enabled(lvl)
}

func (c *redactingCore) With(fields []zapcore.Field) zapcore.Core {
	return &redactingCore{inner: c.inner.With(redactFields(fields))}
}

func (c *redactingCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.inner.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}

	return ce
}

func (c *redactingCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	secrets := loadRedactedValues()
	if len(secrets) == 0 {
		return c.inner.Write(entry, fields)
	}

	entry.Message = redact(entry.Message, secrets)

	return c.inner.Write(entry, redactFields(fields))
}

func (c *redactingCore) Sync() error {
	return c.inner.Sync()
}

// store installs values under redactMu. Empties are dropped and the result is
// sorted longest-first, so an overlapping shorter secret cannot leave a residue
// of a longer one.
func store(values []string) {
	kept := make([]string, 0, len(values))

	for _, v := range values {
		if v != "" {
			kept = append(kept, v)
		}
	}

	slices.SortFunc(kept, func(a, b string) int { return len(b) - len(a) })

	redactedValues.Store(&kept)
}

func loadRedactedValues() []string {
	p := redactedValues.Load()
	if p == nil {
		return nil
	}

	return *p
}

// redactFields returns fields with secrets scrubbed; the input slice is shared
// with sibling cores, so changes go into a copy.
func redactFields(fields []zapcore.Field) []zapcore.Field {
	secrets := loadRedactedValues()
	if len(secrets) == 0 {
		return fields
	}

	var out []zapcore.Field

	for i, f := range fields {
		clean, changed := redactField(f, secrets)
		if !changed {
			continue
		}

		if out == nil {
			out = make([]zapcore.Field, len(fields))
			copy(out, fields)
		}

		out[i] = clean
	}

	if out == nil {
		return fields
	}

	return out
}

func redactField(f zapcore.Field, secrets []string) (zapcore.Field, bool) {
	//nolint:exhaustive // string/byte-string/error carry free text; other types stay numeric or structured
	switch f.Type {
	case zapcore.StringType:
		if clean := redact(f.String, secrets); clean != f.String {
			f.String = clean
			return f, true
		}
	case zapcore.ByteStringType:
		if b, ok := f.Interface.([]byte); ok {
			if clean := redact(string(b), secrets); clean != string(b) {
				f.Interface = []byte(clean)
				return f, true
			}
		}
	case zapcore.ErrorType:
		if err, ok := f.Interface.(error); ok {
			if clean := redact(err.Error(), secrets); clean != err.Error() {
				return zapcore.Field{Key: f.Key, Type: zapcore.StringType, String: clean}, true
			}
		}
	}

	return f, false
}

func redact(s string, secrets []string) string {
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, redactedPlaceholder)
	}

	return s
}
