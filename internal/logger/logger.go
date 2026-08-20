package logger

import (
	"context"
	"io"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// L is the global logger instance. Pre-initialized with a simple console logger
// so it's usable before Init() is called.
var L *zap.Logger

// atom controls the global log level at runtime.
var atom zap.AtomicLevel

// Option configures the logger pipeline built by Init.
type Option func(*initConfig)

type initConfig struct {
	cores []zapcore.Core
}

type ctxKey struct{}

func init() {
	atom = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	core := zapcore.NewCore(enc, zapcore.AddSync(os.Stderr), atom)
	L = zap.New(newRedactingCore(core))
}

// WithConsoleOutput adds Zap's plain-text console encoder writing to w.
func WithConsoleOutput(w io.Writer) Option {
	return func(c *initConfig) {
		enc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
		core := zapcore.NewCore(enc, zapcore.AddSync(w), atom)
		c.cores = append(c.cores, core)
	}
}

// WithJSONOutput adds a JSON encoder writing to w.
func WithJSONOutput(w io.Writer) Option {
	return func(c *initConfig) {
		cfg := zap.NewProductionEncoderConfig()
		cfg.TimeKey = "ts"
		cfg.EncodeTime = zapcore.ISO8601TimeEncoder
		enc := zapcore.NewJSONEncoder(cfg)
		core := zapcore.NewCore(enc, zapcore.AddSync(w), atom)
		c.cores = append(c.cores, core)
	}
}

// WithSessionPrefix wraps all cores with the session prefix middleware.
func WithSessionPrefix() Option {
	return func(c *initConfig) {
		for i, core := range c.cores {
			c.cores[i] = newSessionPrefixCore(core)
		}
	}
}

// Init builds the core pipeline and replaces the global logger.
// WithSessionPrefix must come after output options (it wraps existing cores).
func Init(opts ...Option) {
	cfg := &initConfig{}
	for _, o := range opts {
		o(cfg)
	}

	if len(cfg.cores) == 0 {
		return
	}

	var core zapcore.Core
	if len(cfg.cores) == 1 {
		core = cfg.cores[0]
	} else {
		core = zapcore.NewTee(cfg.cores...)
	}

	L = zap.New(newRedactingCore(core))
}

// SetDebug enables or disables debug logging.
func SetDebug(enabled bool) {
	if enabled {
		atom.SetLevel(zapcore.DebugLevel)
	} else {
		atom.SetLevel(zapcore.InfoLevel)
	}
}

// Named returns a new logger with the given name segment.
func Named(name string) *zap.Logger {
	return L.Named(name)
}

// Ctx returns the logger from the context, or the global logger.
func Ctx(ctx context.Context) *zap.Logger {
	if ctx == nil {
		return L
	}

	if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok && l != nil {
		return l
	}

	return L
}

// With returns a new context with the logger enriched with fields.
func With(ctx context.Context, fields ...zap.Field) context.Context {
	l := Ctx(ctx).With(fields...)
	return context.WithValue(ctx, ctxKey{}, l)
}

// ToContext returns a new context with the given logger.
func ToContext(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}
