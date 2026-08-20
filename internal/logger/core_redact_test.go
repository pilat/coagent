package logger

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func newRedactObserver(t *testing.T, secrets ...string) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	t.Cleanup(func() { SetRedactedValues(nil) })

	SetRedactedValues(secrets)

	inner, logs := observer.New(zapcore.DebugLevel)

	return zap.New(newRedactingCore(inner)), logs
}

func TestRedactingCore_Message(t *testing.T) {
	log, logs := newRedactObserver(t, "s3cr3t-token")

	log.Info("posting to https://api.telegram.org/bots3cr3t-token/getUpdates")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "posting to https://api.telegram.org/bot[REDACTED]/getUpdates", logs.All()[0].Message)
}

func TestRedactingCore_StringField(t *testing.T) {
	log, logs := newRedactObserver(t, "s3cr3t-token")

	log.Info("request", zap.String("url", "https://x/bots3cr3t-token/y"), zap.String("clean", "untouched"))

	require.Equal(t, 1, logs.Len())
	ctx := logs.All()[0].ContextMap()
	assert.Equal(t, "https://x/bot[REDACTED]/y", ctx["url"])
	assert.Equal(t, "untouched", ctx["clean"])
}

func TestRedactingCore_ErrorField(t *testing.T) {
	log, logs := newRedactObserver(t, "s3cr3t-token")

	err := fmt.Errorf("call telegram: Post %q: context canceled", "https://api.telegram.org/bots3cr3t-token/getUpdates")
	log.Warn("getupdates_failed", zap.Error(err))

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	require.Len(t, entry.Context, 1)
	assert.Equal(t, zapcore.StringType, entry.Context[0].Type)
	assert.Equal(
		t,
		`call telegram: Post "https://api.telegram.org/bot[REDACTED]/getUpdates": context canceled`,
		entry.ContextMap()["error"],
	)
}

func TestRedactingCore_WithFields(t *testing.T) {
	log, logs := newRedactObserver(t, "s3cr3t-token")

	log.With(zap.String("endpoint", "bots3cr3t-token/send")).Info("attached")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "bot[REDACTED]/send", logs.All()[0].ContextMap()["endpoint"])
}

func TestRedactingCore_NoSecretsPassthrough(t *testing.T) {
	log, logs := newRedactObserver(t)

	err := fmt.Errorf("plain failure")
	log.Info("hello s3cr3t-token", zap.Error(err))

	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "hello s3cr3t-token", entry.Message)
	require.Len(t, entry.Context, 1)
	assert.Equal(t, zapcore.ErrorType, entry.Context[0].Type)
}

func TestRedactingCore_OverlappingSecretsLongestFirst(t *testing.T) {
	log, logs := newRedactObserver(t, "abc", "abcdef")

	log.Info("token=abcdef and short=abc")

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "token=[REDACTED] and short=[REDACTED]", logs.All()[0].Message)
}

func TestRedactHelper(t *testing.T) {
	t.Cleanup(func() { SetRedactedValues(nil) })

	assert.Equal(t, "no secrets set", Redact("no secrets set"))

	SetRedactedValues([]string{"s3cr3t-token"})
	assert.Equal(t, "start [REDACTED] failed", Redact("start s3cr3t-token failed"))
}

func TestInitConsolePipelineRedacts(t *testing.T) {
	prev := L
	t.Cleanup(func() {
		L = prev
		SetRedactedValues(nil)
	})

	var buf bytes.Buffer
	Init(WithConsoleOutput(&buf), WithSessionPrefix())
	SetRedactedValues([]string{"s3cr3t-token"})

	tokenErr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bots3cr3t-token/getUpdates",
		Err: errors.New("context canceled"),
	}
	L.Warn("getupdates_failed", zap.Error(tokenErr))

	out := buf.String()
	assert.NotContains(t, out, "s3cr3t-token")
	assert.Contains(t, out, "WARN\tgetupdates_failed")
	assert.Contains(t, out, "[REDACTED]")
}

func TestRedactingCore_ByteStringField(t *testing.T) {
	log, logs := newRedactObserver(t, "s3cr3t-token")

	log.Info("raw", zap.ByteString("body", []byte("key=s3cr3t-token")))

	require.Equal(t, 1, logs.Len())
	assert.Equal(t, "key=[REDACTED]", logs.All()[0].ContextMap()["body"])
}
