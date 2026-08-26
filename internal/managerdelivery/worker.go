package managerdelivery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	baseRetry = 3 * time.Second
	maxRetry  = 60 * time.Second
)

var ErrNoItem = errors.New("no deliverable manager output")

type RetryPendingError struct{ NextAt time.Time }

func (e *RetryPendingError) Error() string { return "manager output retry is not due" }

type Item struct {
	ID        int64
	AttemptID string
	Attempts  int64
	Payload   any
}

type Result struct {
	MessageIDs   []string
	SessionPatch map[string]any
	RetryAfter   time.Duration
	Retryable    bool
	Error        string
}

type Queue interface {
	Claim(context.Context) (*Item, error)
	Ack(context.Context, *Item, Result) error
	Retry(context.Context, *Item, string, time.Time) error
	Block(context.Context, *Item, string) error
}

type Transport interface {
	Deliver(context.Context, *Item) Result
}

type Worker interface {
	Start(context.Context)
	Wake()
	Stop(context.Context) error
}

type worker struct {
	queue     Queue
	transport Transport
	wake      chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	// stopped pins a Stop that ran before Start, so a later Start cannot spawn
	// a goroutine the earlier Stop already gave up on.
	stopped bool
}

var _ Worker = (*worker)(nil)

func New(queue Queue, transport Transport) Worker {
	return &worker{queue: queue, transport: transport, wake: make(chan struct{}, 1)}
}

func (w *worker) Start(ctx context.Context) {
	w.mu.Lock()
	if w.done != nil || w.stopped {
		w.mu.Unlock()
		return
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.cancel = cancel
	w.done = make(chan struct{})
	done := w.done
	w.mu.Unlock()

	go func() {
		defer close(done)

		w.run(runCtx)
	}()

	w.Wake()
}

func (w *worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) Stop(ctx context.Context) error {
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

func (w *worker) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		delay := w.drain(ctx)
		if ctx.Err() != nil {
			return
		}

		if delay <= 0 {
			delay = time.Minute
		}

		timer.Reset(delay)

		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// deliver runs one transport attempt. A panicking adapter becomes an ordinary
// retryable failure instead of a dead daemon or a head stranded in delivering;
// the stored retry error is the observable trace.
func (w *worker) deliver(ctx context.Context, item *Item) Result {
	var result Result

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = Result{Retryable: true, Error: fmt.Sprintf("delivery panicked: %v", recovered)}
			}
		}()

		result = w.transport.Deliver(ctx, item)
	}()

	return result
}

func (w *worker) drain(ctx context.Context) time.Duration {
	for {
		item, err := w.queue.Claim(ctx)
		if errors.Is(err, ErrNoItem) {
			return 0
		}

		var retryPending *RetryPendingError
		if errors.As(err, &retryPending) {
			return max(time.Until(retryPending.NextAt), 0)
		}

		if err != nil {
			return baseRetry
		}

		result := w.deliver(ctx, item)
		if ctx.Err() != nil {
			w.releaseClaim(ctx, item)
			return 0
		}

		if result.Error == "" {
			if err := w.queue.Ack(ctx, item, result); err != nil {
				w.releaseClaim(ctx, item)
				return baseRetry
			}

			continue
		}

		if !result.Retryable {
			if err := w.queue.Block(ctx, item, result.Error); err != nil {
				w.releaseClaim(ctx, item)
			}

			return 0
		}

		delay := retryDelay(item.Attempts, result.RetryAfter)
		if err := w.queue.Retry(ctx, item, result.Error, time.Now().UTC().Add(delay)); err != nil {
			w.releaseClaim(ctx, item)
			return baseRetry
		}

		return delay
	}
}

// releaseClaim returns a claimed row to its durable queue before the worker
// exits. A stopped manager must not leave a delivery attempt stranded until a
// daemon-wide restart sweep happens to recover it.
func (w *worker) releaseClaim(ctx context.Context, item *Item) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	_ = w.queue.Retry(ctx, item, "manager stopped before acknowledgement", time.Now().UTC())
}

func retryDelay(attempts int64, retryAfter time.Duration) time.Duration {
	delay := baseRetry
	for i := int64(1); i < attempts && delay < maxRetry; i++ {
		delay *= 2
	}

	if delay > maxRetry {
		delay = maxRetry
	}

	if retryAfter > delay {
		return retryAfter
	}

	return delay
}
