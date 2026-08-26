package managerdelivery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkerCoalescesWakesAndDrainsInOrder(t *testing.T) {
	queue := &recordingQueue{items: []*Item{{ID: 1, AttemptID: "a", Attempts: 1}, {ID: 2, AttemptID: "b", Attempts: 1}}}
	worker := New(queue, TransportFunc(func(context.Context, *Item) Result { return Result{} }))
	worker.Start(t.Context())
	for range 1_000 {
		worker.Wake()
	}
	require.Eventually(t, func() bool { return queue.ackCount() == 2 }, time.Second, time.Millisecond)
	require.NoError(t, worker.Stop(t.Context()))
	assert.Equal(t, []int64{1, 2}, queue.acked)
}

func TestWorkerReturnsClaimToRetryWhenStopped(t *testing.T) {
	started := make(chan struct{})
	queue := &recordingQueue{items: []*Item{{ID: 1, AttemptID: "a", Attempts: 1}}}
	worker := New(queue, TransportFunc(func(ctx context.Context, _ *Item) Result {
		close(started)
		<-ctx.Done()
		return Result{Retryable: true, Error: ctx.Err().Error()}
	}))
	worker.Start(t.Context())
	<-started
	require.NoError(t, worker.Stop(t.Context()))
	assert.Equal(t, []int64{1}, queue.retried)
}

func TestWorkerKeepsTheHeadRetryDeadlineAfterWake(t *testing.T) {
	due := time.Now().Add(4 * time.Second)
	worker := New(futureHeadQueue{due: due}, TransportFunc(func(context.Context, *Item) Result {
		t.Fatal("future head must not reach transport")
		return Result{}
	})).(*worker)
	delay := worker.drain(t.Context())
	assert.InDelta(t, time.Until(due).Seconds(), delay.Seconds(), 0.1)
}

type futureHeadQueue struct{ due time.Time }

func (q futureHeadQueue) Claim(context.Context) (*Item, error) {
	return nil, &RetryPendingError{NextAt: q.due}
}

func (futureHeadQueue) Ack(context.Context, *Item, Result) error              { return nil }
func (futureHeadQueue) Retry(context.Context, *Item, string, time.Time) error { return nil }
func (futureHeadQueue) Block(context.Context, *Item, string) error            { return nil }

type TransportFunc func(context.Context, *Item) Result

func (fn TransportFunc) Deliver(ctx context.Context, item *Item) Result { return fn(ctx, item) }

type recordingQueue struct {
	mu      sync.Mutex
	items   []*Item
	acked   []int64
	retried []int64
}

func (q *recordingQueue) Claim(context.Context) (*Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, ErrNoItem
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, nil
}

func (q *recordingQueue) Ack(_ context.Context, item *Item, _ Result) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acked = append(q.acked, item.ID)
	return nil
}

func (q *recordingQueue) Retry(_ context.Context, item *Item, _ string, _ time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retried = append(q.retried, item.ID)
	return nil
}
func (q *recordingQueue) Block(context.Context, *Item, string) error { return nil }

func (q *recordingQueue) ackCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.acked)
}
