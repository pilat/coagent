package lsp

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type protocolModel struct {
	live    bool
	pending map[int64]struct{}
}

type cancellationModel struct {
	exited          bool
	cancelNotified  bool
	pendingRequests int
}

type cancellationState uint8

const (
	cancellationQueued cancellationState = iota
	cancellationActive
	cancellationComplete
	cancellationNotificationTimeout = time.Second
)

func newProtocolModel() protocolModel {
	return protocolModel{live: true, pending: make(map[int64]struct{})}
}

func TestClientCancellationModel(t *testing.T) {
	tests := []struct {
		name  string
		state cancellationState
		model cancellationModel
	}{
		{name: "queued", state: cancellationQueued, model: cancellationModel{}},
		{name: "active", state: cancellationActive, model: cancellationModel{exited: true}},
		{
			name:  "acknowledged",
			state: cancellationComplete,
			model: cancellationModel{cancelNotified: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := runCancellationScenario(t, tt.state)
			assert.Equal(t, tt.model, actual)
		})
	}
}

func FuzzClientProtocol(f *testing.F) {
	f.Add([]byte{0, 0, 2, 1, 3, 4, 5})
	f.Add([]byte{0, 1, 5, 4, 2, 3})
	f.Fuzz(func(t *testing.T, trace []byte) {
		c := newClient()
		c.stdin = protocolWriteCloser{Writer: io.Discard}
		model := newProtocolModel()
		for _, command := range trace {
			applyProtocolCommand(t, c, &model, command)
		}
		c.failWriter()
	})
}

func applyProtocolCommand(t *testing.T, c *client, model *protocolModel, command byte) {
	t.Helper()
	id := int64(command>>3) + 1
	switch command % 6 {
	case 0:
		if model.live {
			c.pendingMu.Lock()
			c.pending[id] = make(chan rpcResult, 1)
			c.pendingMu.Unlock()
			model.pending[id] = struct{}{}
		}
	case 1:
		c.removePending(id)
		delete(model.pending, id)
	case 2:
		c.routeResponse(context.Background(), map[string]json.RawMessage{
			"id":     json.RawMessage(jsonNumber(id)),
			"result": json.RawMessage("null"),
		})
		delete(model.pending, id)
	case 3:
		c.routeServerMethod(context.Background(), map[string]json.RawMessage{
			"id":     json.RawMessage(`"server"`),
			"method": json.RawMessage(`"workspace/configuration"`),
			"params": json.RawMessage(`{"items":[]}`),
		})
	case 4:
		c.cleanupPending()
		model.live = false
		model.pending = make(map[int64]struct{})
	case 5:
		c.routeResponse(context.Background(), map[string]json.RawMessage{
			"error": json.RawMessage(`{"code":-32001,"message":"failed"}`),
			"id":    json.RawMessage(jsonNumber(id)),
		})
		delete(model.pending, id)
	}

	c.pendingMu.Lock()
	actual := len(c.pending)
	c.pendingMu.Unlock()
	assert.Equal(t, len(model.pending), actual)
}

func runCancellationScenario(t *testing.T, state cancellationState) cancellationModel {
	t.Helper()
	c := newManualWriterClient()
	reached := make(chan struct{})
	release := make(chan struct{})
	notifications := make(chan []byte, 1)
	go runCancellationWriter(c, state, reached, release, notifications)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.call(ctx, "textDocument/hover", nil, nil) }()
	<-reached
	cancel()
	if state == cancellationComplete {
		close(release)
	}

	require.ErrorIs(t, <-result, context.Canceled)
	actual := cancellationModel{exited: c.hasExited()}
	if state == cancellationComplete {
		actual.cancelNotified = isCancelNotification(t, notifications)
	} else {
		actual.cancelNotified = isCancelOutbound(t, c.writer)
	}

	c.pendingMu.Lock()
	actual.pendingRequests = len(c.pending)
	c.pendingMu.Unlock()

	return actual
}

func newManualWriterClient() *client {
	c := newClient()
	c.stdin = protocolWriteCloser{Writer: io.Discard}
	c.writer = make(chan *outbound, 2)
	c.writerDone = make(chan struct{})
	c.writerOnce.Do(func() {})

	return c
}

func runCancellationWriter(
	c *client,
	state cancellationState,
	reached chan<- struct{},
	release <-chan struct{},
	notifications chan<- []byte,
) {
	request := <-c.writer
	if state == cancellationQueued {
		close(reached)
		return
	}

	request.start()
	if state == cancellationActive {
		close(reached)
		return
	}

	request.finish()
	close(reached)
	<-release
	request.done <- writeResult{}

	request = <-c.writer
	notifications <- request.data
	request.start()
	request.finish()
	request.done <- writeResult{}
}

func isCancelNotification(t *testing.T, notifications <-chan []byte) bool {
	t.Helper()
	select {
	case data := <-notifications:
		return isCancelData(t, data)
	case <-time.After(cancellationNotificationTimeout):
		return false
	}
}

func isCancelOutbound(t *testing.T, outbounds <-chan *outbound) bool {
	t.Helper()
	select {
	case outbound := <-outbounds:
		return isCancelData(t, outbound.data)
	case <-time.After(cancellationNotificationTimeout):
		return false
	}
}

func isCancelData(t *testing.T, data []byte) bool {
	t.Helper()
	var notification struct {
		Method string `json:"method"`
	}
	require.NoError(t, json.Unmarshal(data, &notification))

	return notification.Method == "$/cancelRequest"
}

func jsonNumber(value int64) string { return strconv.FormatInt(value, 10) }

type protocolWriteCloser struct{ io.Writer }

func (protocolWriteCloser) Close() error { return nil }
