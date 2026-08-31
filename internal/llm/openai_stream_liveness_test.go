package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
)

// withShortDeadlines overrides the package SSE deadlines. The test calling it
// must not be parallel: parallel top-level tests resume only after sequential
// ones finish, so the restored values are never observed mutated.
func withShortDeadlines(t *testing.T, first, idle time.Duration) {
	t.Helper()

	oldFirst, oldIdle := sseFirstEventDeadline, sseIdleEventDeadline
	sseFirstEventDeadline, sseIdleEventDeadline = first, idle
	t.Cleanup(func() { sseFirstEventDeadline, sseIdleEventDeadline = oldFirst, oldIdle })
}

func newStreamTestClient(t *testing.T, url string) Client {
	t.Helper()

	client, err := newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: url, APIKey: "key", Model: config.ModelEntry{ID: "test-model"}, IsOpenRouter: true,
	})
	require.NoError(t, err)

	return client
}

// commentOnlyStreamHandler writes one OpenRouter-style keep-alive comment and
// then goes silent — the incident shape that no byte-idle timer can catch.
func commentOnlyStreamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(": OPENROUTER PROCESSING\n\n"))
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

func TestOpenRouterStreamFirstEventDeadlineFires(t *testing.T) {
	withShortDeadlines(t, 80*time.Millisecond, 80*time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(commentOnlyStreamHandler))
	defer srv.Close()

	client := newStreamTestClient(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.Chat(ctx, "", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, errStreamIdle)
	require.ErrorContains(t, err, "no first event")
	assert.Less(t, time.Since(start), 5*time.Second, "the liveness deadline fired, not the context")
}

func TestOpenRouterStreamIdleDeadlineFiresBetweenEvents(t *testing.T) {
	withShortDeadlines(t, 80*time.Millisecond, 80*time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := newStreamTestClient(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Chat(ctx, "", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, errStreamIdle)
	require.ErrorContains(t, err, "stream went idle")
}

func TestOpenRouterStreamSurvivesCommentKeepAlives(t *testing.T) {
	withShortDeadlines(t, 2*time.Second, 150*time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		_, _ = w.Write([]byte(": OPENROUTER PROCESSING\n\n"))
		w.(http.Flusher).Flush()

		for range 3 {
			time.Sleep(30 * time.Millisecond)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
			w.(http.Flusher).Flush()
		}

		_, _ = w.Write([]byte(
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := newStreamTestClient(t, srv.URL)

	resp, err := client.Chat(context.Background(), "", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "xxx", resp.Text)
}

func TestRetryStreamIdleRetriesExactlyOnce(t *testing.T) {
	withShortDeadlines(t, 80*time.Millisecond, 80*time.Millisecond)

	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		commentOnlyStreamHandler(w, r)
	}))
	defer srv.Close()

	client := newRetryableClient(newStreamTestClient(t, srv.URL), 0).(*retryableClient)
	client.baseDelay = time.Millisecond
	client.maxDelay = time.Millisecond

	_, err := client.Chat(context.Background(), "", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, errStreamIdle)
	assert.Equal(t, 2, requests, "exactly one retry after the idle sentinel")
}

func TestStreamLivenessRecordsFirstEventOnTrace(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	trace := newRequestTrace(10)
	live := newStreamLiveness(pw, trace)
	live.event()
	live.stop()

	assert.Positive(t, trace.firstEvent.Load())
	assert.False(t, live.first)
}

func TestRequestTraceHooksPopulate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	body := bytes.Repeat([]byte("a"), 4096)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader(body))
	require.NoError(t, err)

	trace := newRequestTrace(len(body))
	req = trace.attach(req)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	assert.Positive(t, trace.wroteHeaders.Load())
	assert.Positive(t, trace.wroteRequest.Load())
	assert.Positive(t, trace.firstByte.Load())
	assert.Zero(t, trace.firstEvent.Load(), "non-streaming requests have no SSE event")
	assert.LessOrEqual(t, trace.wroteHeaders.Load(), trace.wroteRequest.Load())
	assert.LessOrEqual(t, trace.wroteRequest.Load(), trace.firstByte.Load())
}

func TestLogRequestTraceEmitsUploadAndStallSplit(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	log := zap.New(core)

	trace := newRequestTrace(1024)
	now := time.Now()
	trace.wroteHeaders.Store(now.Add(-300 * time.Millisecond).UnixNano())
	trace.wroteRequest.Store(now.Add(-200 * time.Millisecond).UnixNano())
	trace.firstByte.Store(now.Add(-190 * time.Millisecond).UnixNano())
	trace.firstEvent.Store(now.Add(-10 * time.Millisecond).UnixNano())

	logRequestTrace(log, trace, errors.New("boom"))

	entries := logs.FilterMessage("request_trace_failed").All()
	require.Len(t, entries, 1)

	fields := entries[0].ContextMap()
	assert.EqualValues(t, 1024, fields["body_bytes"])
	assert.InDelta(t, 100, fields["write_ms"], 5)
	assert.InDelta(t, 10, fields["first_byte_ms"], 5)
	assert.InDelta(t, 180, fields["first_event_ms"], 5)
}

func TestSpanMsMissingEndpoint(t *testing.T) {
	assert.Equal(t, int64(-1), spanMs(0, 123))
	assert.Equal(t, int64(-1), spanMs(123, 0))
	assert.Equal(t, int64(50), spanMs(1000, 51_000_000))
}

func TestOneShotClassIdleSentinelOnly(t *testing.T) {
	class, ok := oneShotClass(fmt.Errorf("read response stream: %w", errStreamIdle))
	assert.True(t, ok)
	assert.Equal(t, "sse-idle", class)

	_, ok = oneShotClass(errors.New("read response stream: read SSE line: context deadline exceeded"))
	assert.False(t, ok, "a plain transport timeout keeps the default retry ladder")

	_, ok = oneShotClass(errors.New("openrouter api error: status=502, body=bad gateway"))
	assert.False(t, ok)
}
