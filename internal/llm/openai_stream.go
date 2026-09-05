package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
)

type oaiStreamChunk struct {
	Choices []oaiStreamChoice `json:"choices"`
	Usage   oaiUsage          `json:"usage"`
	Error   json.RawMessage   `json:"error,omitempty"`
}

type oaiStreamChoice struct {
	Index        int            `json:"index"`
	Delta        oaiStreamDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
}

type oaiStreamDelta struct {
	Content          json.RawMessage     `json:"content"`
	ToolCalls        []oaiStreamToolCall `json:"tool_calls,omitempty"`
	Reasoning        *string             `json:"reasoning"`
	ReasoningContent *string             `json:"reasoning_content"`
	ReasoningDetails json.RawMessage     `json:"reasoning_details,omitempty"`
}

type oaiStreamToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

type oaiStreamAggregate struct {
	content          strings.Builder
	reasoning        strings.Builder
	reasoningDetails []json.RawMessage
	toolCalls        map[int]*oaiToolCall
	finishReason     string
	usage            oaiUsage
	seenChoice       bool
}

func (c *openaiClient) makeStreamingRequest(
	log *zap.Logger,
	req *http.Request,
	trace *requestTrace,
	start time.Time,
) (*llmwire.Response, error) {
	resp, err := c.httpClient.Do(req) //nolint:gosec // base URL is operator-configured, not user input
	if err != nil {
		return nil, fmt.Errorf("%s api request failed: %w", c.provider, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("read error response body: %w", readErr)
		}

		return nil, fmt.Errorf("%s api error: status=%d, body=%s", c.provider, resp.StatusCode, string(body))
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("read response body: %w", readErr)
		}

		return c.parseResponseBody(log, body, start)
	}

	// The liveness clock arms with the body in hand: upload time is excluded
	// and observed separately on the request trace (D11).
	live := newStreamLiveness(resp.Body, trace)

	aggregate, err := readOpenAIStream(resp.Body, live)
	if err != nil {
		return nil, fmt.Errorf("read response stream: %w", err)
	}

	return c.finishStreamingResponse(log, aggregate, start)
}

func (c *openaiClient) finishStreamingResponse(
	log *zap.Logger,
	aggregate *oaiStreamAggregate,
	start time.Time,
) (*llmwire.Response, error) {
	message, err := aggregate.message()
	if err != nil {
		return nil, err
	}

	result, err := c.parseMessage(message, aggregate.finishReason)
	if err != nil {
		return nil, err
	}

	c.logResponse(log, aggregate.finishReason, result, time.Since(start).Milliseconds())

	completionResp := oaiResponse{Usage: aggregate.usage}
	if err := c.checkEmptyResponse(log, result, aggregate.finishReason, nil, &completionResp); err != nil {
		return nil, err
	}

	attachUsage(result, extractUsage(&completionResp, c.provider, c.model, c.pricing))
	logServerToolUse(log, aggregate.usage)

	return result, nil
}

var (
	// sseFirstEventDeadline bounds the silent wait for the first payload event.
	// Provider keep-alives are SSE comments and never count as events.
	sseFirstEventDeadline = 120 * time.Second
	// sseIdleEventDeadline bounds the gap between two payload events.
	sseIdleEventDeadline = 120 * time.Second
)

// errStreamIdle must stay a typed sentinel: the retry classifier is a string
// ladder that cannot count, and the enforced body close surfaces as a
// "read on closed response body" indistinguishable from a transport abort.
var errStreamIdle = errors.New("sse stream delivered no payload event within its deadline")

// streamLiveness arms a per-event deadline over a streaming body. The deadline
// counts payload events, not bytes, so comment keep-alives never reset it.
// Firing closes the body to unblock the pending read; the flag lets the reader
// replace the resulting close error with errStreamIdle.
type streamLiveness struct {
	body  io.Closer
	trace *requestTrace
	timer *time.Timer
	fired atomic.Bool
	first bool
}

func newStreamLiveness(body io.Closer, trace *requestTrace) *streamLiveness {
	l := &streamLiveness{body: body, trace: trace, first: true}
	l.arm(sseFirstEventDeadline)

	return l
}

// arm starts the deadline; any previous timer must already be stopped.
func (l *streamLiveness) arm(d time.Duration) {
	l.timer = time.AfterFunc(d, func() {
		l.fired.Store(true)
		_ = l.body.Close()
	})
}

// event records one delivered payload event: the first-event phase ends and
// the idle deadline re-arms.
func (l *streamLiveness) event() {
	if l.first {
		l.first = false

		if l.trace != nil {
			l.trace.firstEvent.Store(time.Now().UnixNano())
		}
	}

	l.timer.Stop()
	l.arm(sseIdleEventDeadline)
}

func (l *streamLiveness) stop() {
	l.timer.Stop()
}

func (l *streamLiveness) phase() string {
	if l.first {
		return "no first event"
	}

	return "stream went idle"
}

func readOpenAIStream(body io.Reader, live *streamLiveness) (*oaiStreamAggregate, error) {
	if live != nil {
		defer live.stop()
	}

	reader := bufio.NewReader(body)
	aggregate := &oaiStreamAggregate{toolCalls: make(map[int]*oaiToolCall)}

	for {
		data, err := readSSEData(reader)
		if err != nil {
			if live != nil && live.fired.Load() {
				return nil, fmt.Errorf("%w: %s", errStreamIdle, live.phase())
			}

			if errors.Is(err, io.EOF) {
				return nil, errors.New("stream ended before [DONE]")
			}

			return nil, err
		}

		if bytes.Equal(data, []byte("[DONE]")) {
			if !aggregate.seenChoice {
				return nil, errors.New("openrouter api returned no streamed choices")
			}

			return aggregate, nil
		}

		// Payload events re-arm the deadline; [DONE] returns above and the
		// deferred stop retires the timer instead of re-arming it.
		if live != nil {
			live.event()
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}

		if len(chunk.Error) > 0 {
			msg := (&oaiResponse{Error: chunk.Error}).errorMessage()
			return nil, fmt.Errorf("openrouter api stream error: %s", msg)
		}

		if err := aggregate.add(chunk); err != nil {
			return nil, err
		}
	}
}

func readSSEData(reader *bufio.Reader) ([]byte, error) {
	var data [][]byte

	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})

		if len(line) == 0 && len(data) > 0 {
			return bytes.Join(data, []byte{'\n'}), nil
		}

		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(payload, []byte{' '}))
		}

		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				return bytes.Join(data, []byte{'\n'}), nil
			}

			return nil, fmt.Errorf("read SSE line: %w", err)
		}
	}
}

func (a *oaiStreamAggregate) add(chunk oaiStreamChunk) error {
	if chunk.Usage != (oaiUsage{}) {
		a.usage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}

		a.seenChoice = true
		a.addDelta(choice.Delta)

		if choice.FinishReason != "" {
			a.finishReason = choice.FinishReason
		}
	}

	return a.addReasoningDetails(chunk.Choices)
}

func (a *oaiStreamAggregate) addDelta(delta oaiStreamDelta) {
	if len(delta.Content) > 0 && !bytes.Equal(delta.Content, []byte("null")) {
		a.content.WriteString((&oaiMessage{RawContent: delta.Content}).content())
	}

	if delta.Reasoning != nil {
		a.reasoning.WriteString(*delta.Reasoning)
	}

	if delta.ReasoningContent != nil {
		a.reasoning.WriteString(*delta.ReasoningContent)
	}

	for _, fragment := range delta.ToolCalls {
		call := a.toolCalls[fragment.Index]
		if call == nil {
			call = &oaiToolCall{Index: fragment.Index}
			a.toolCalls[fragment.Index] = call
		}

		if fragment.ID != "" {
			call.ID = fragment.ID
		}

		if fragment.Type != "" {
			call.Type = fragment.Type
		}

		call.Function.Name += fragment.Function.Name
		call.Function.Arguments += fragment.Function.Arguments
	}
}

func (a *oaiStreamAggregate) addReasoningDetails(choices []oaiStreamChoice) error {
	for _, choice := range choices {
		if choice.Index != 0 || len(choice.Delta.ReasoningDetails) == 0 ||
			bytes.Equal(choice.Delta.ReasoningDetails, []byte("null")) {
			continue
		}

		var details []json.RawMessage
		if err := json.Unmarshal(choice.Delta.ReasoningDetails, &details); err != nil {
			return fmt.Errorf("decode reasoning details: %w", err)
		}

		a.reasoningDetails = append(a.reasoningDetails, details...)
	}

	return nil
}

func (a *oaiStreamAggregate) message() (*oaiMessage, error) {
	content, err := json.Marshal(a.content.String())
	if err != nil {
		return nil, fmt.Errorf("encode streamed content: %w", err)
	}

	reasoning := a.reasoning.String()
	message := &oaiMessage{RawContent: content, ReasoningContent: &reasoning}

	indices := make([]int, 0, len(a.toolCalls))
	for index := range a.toolCalls {
		indices = append(indices, index)
	}

	sort.Ints(indices)

	for _, index := range indices {
		message.ToolCalls = append(message.ToolCalls, *a.toolCalls[index])
	}

	if len(a.reasoningDetails) > 0 {
		message.ReasoningDetails, err = json.Marshal(a.reasoningDetails)
		if err != nil {
			return nil, fmt.Errorf("encode reasoning details: %w", err)
		}
	}

	return message, nil
}
