package managerdelivery

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const (
	chatBatchSize = 100
	chatFallback  = 5 * time.Second
)

var _ ChatLoop = (*chatLoop)(nil)

type chatLoop struct {
	port      ChatPort
	transport ChatTransport
	wake      chan struct{}
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	stopped   bool
	focus     atomic.Int64

	initialized bool
	lastFocus   int64
	prelude     []string
	failures    int64
}

// NewChat creates one delivery loop for a bound chat, initially at assistant #0.
func NewChat(port ChatPort, transport ChatTransport) ChatLoop {
	return &chatLoop{port: port, transport: transport, wake: make(chan struct{}, 1)}
}

func (w *chatLoop) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.done != nil || w.stopped {
		return
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.cancel = cancel
	w.done = make(chan struct{})

	go func() {
		defer close(w.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(runCtx).Error("chat loop panicked", zap.Any("recovered", recovered), zap.Stack("stack"))
			}
		}()

		w.run(runCtx)
	}()
}

func (w *chatLoop) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *chatLoop) Focus() int64 { return w.focus.Load() }

func (w *chatLoop) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.stopped = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()

	if done == nil {
		return nil
	}

	cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *chatLoop) run(ctx context.Context) {
	for ctx.Err() == nil {
		delay := w.pass(ctx)
		if ctx.Err() != nil {
			return
		}

		if !w.wait(ctx, delay) {
			return
		}
	}
}

func (w *chatLoop) wait(ctx context.Context, delay time.Duration) bool {
	var wake <-chan struct{}

	if delay == 0 {
		delay, wake = chatFallback, w.wake
	}

	// Notifications cannot bypass a retry-after or turn a dead chat into a hot loop.
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-wake:
	case <-timer.C:
	}

	return true
}

func (w *chatLoop) pass(ctx context.Context) time.Duration {
	focus, err := w.port.Focus(ctx)
	if err != nil {
		return w.failed(ctx, SendResult{Class: SendChatFailure, Reason: err.Error()})
	}

	w.focus.Store(focus)

	if err := w.prepare(ctx, focus); err != nil {
		return w.failed(ctx, SendResult{Class: SendChatFailure, Reason: err.Error()})
	}

	for len(w.prelude) > 0 {
		result := w.send(ctx, w.prelude[0])
		if result.Class != SendOK {
			if result.Class == SendRowPermanent {
				result.Class = SendChatFailure
			}

			return w.failed(ctx, result)
		}

		w.prelude = w.prelude[1:]
		w.failures = 0
	}

	changed, result := w.tail(ctx, focus)
	if result.Class != SendOK {
		return w.failed(ctx, result)
	}

	if changed {
		w.Wake()

		return 0
	}

	killed, err := w.port.Killed(ctx, focus)
	if err != nil {
		return w.failed(ctx, SendResult{Class: SendChatFailure, Reason: err.Error()})
	}

	if !killed {
		return 0
	}

	// A kill commits its close row atomically; only a later empty read proves it drained.
	changed, result = w.tail(ctx, focus)
	if result.Class != SendOK {
		return w.failed(ctx, result)
	}

	if changed {
		w.Wake()

		return 0
	}

	if _, err := w.port.ResetFocus(ctx, focus); err != nil {
		return w.failed(ctx, SendResult{Class: SendChatFailure, Reason: err.Error()})
	}

	w.Wake()

	return 0
}

func (w *chatLoop) tail(ctx context.Context, focus int64) (bool, SendResult) {
	for ctx.Err() == nil {
		rows, err := w.port.Outputs(ctx, focus, chatBatchSize)
		if err != nil {
			return false, SendResult{Class: SendChatFailure, Reason: err.Error()}
		}

		if len(rows) == 0 {
			return false, SendResult{}
		}

		for _, row := range rows {
			if result := w.deliverRow(ctx, row); result.Class != SendOK {
				return false, result
			}

			if ctx.Err() != nil {
				return false, SendResult{Class: SendTransient, Reason: ctx.Err().Error()}
			}

			if err := w.port.MarkSeen(ctx, focus, row.ID); err != nil {
				return false, SendResult{Class: SendChatFailure, Reason: err.Error()}
			}

			w.failures = 0

			current, err := w.port.Focus(ctx)
			if err != nil {
				return false, SendResult{Class: SendChatFailure, Reason: err.Error()}
			}

			if current != focus {
				return true, SendResult{}
			}
		}
	}

	return false, SendResult{Class: SendTransient, Reason: ctx.Err().Error()}
}

func (w *chatLoop) deliverRow(ctx context.Context, row ChatRow) SendResult {
	if row.Type == "session_opened" || row.Type == "session_replaced" {
		return SendResult{}
	}

	result := w.send(ctx, row.Content)
	if result.Class != SendRowPermanent {
		return result
	}

	probe := w.send(ctx, fmt.Sprintf("⚠ output #%d not delivered: %s", row.ID, result.Reason))
	if probe.Class == SendRowPermanent {
		probe.Class = SendChatFailure
	}

	return probe
}

func (w *chatLoop) send(ctx context.Context, text string) SendResult {
	var result SendResult

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = SendResult{Class: SendTransient, Reason: fmt.Sprintf("send panicked: %v", recovered)}
			}
		}()

		result = w.transport.Send(ctx, text)
	}()

	return result
}

func (w *chatLoop) failed(ctx context.Context, result SendResult) time.Duration {
	w.failures++

	if result.Class == SendChatFailure {
		logger.Ctx(ctx).Warn("chat delivery paused", zap.String("reason", result.Reason))
	}

	return retryDelay(w.failures, result.RetryAfter)
}
