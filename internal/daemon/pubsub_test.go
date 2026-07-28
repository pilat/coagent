package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionevent"
)

func TestPubSub_MultipleSubscribers(t *testing.T) {
	ps := newPubSub()

	ch1 := ps.Subscribe(int64(1))
	ch2 := ps.Subscribe(int64(1))

	n := sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "hello"}
	ps.Publish(int64(1), n)

	select {
	case got := <-ch1:
		assert.Equal(t, "hello", got.Message)
	case <-time.After(time.Second):
		t.Fatal("ch1 timeout")
	}

	select {
	case got := <-ch2:
		assert.Equal(t, "hello", got.Message)
	case <-time.After(time.Second):
		t.Fatal("ch2 timeout")
	}
}

func TestPubSub_GlobalSubscriber(t *testing.T) {
	ps := newPubSub()

	ch := ps.SubscribeAll()

	ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "from s1"})
	ps.Publish(int64(2), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "from s2"})

	var messages []string
	var sessionIDs []int64
	for range 2 {
		select {
		case got := <-ch:
			messages = append(messages, got.Notification.Message)
			sessionIDs = append(sessionIDs, got.SessionID)
		case <-time.After(time.Second):
			t.Fatal("global timeout")
		}
	}

	assert.Contains(t, messages, "from s1")
	assert.Contains(t, messages, "from s2")
	assert.Contains(t, sessionIDs, int64(1))
	assert.Contains(t, sessionIDs, int64(2))
}

func TestPubSub_SlowSubscriber(t *testing.T) {
	ps := newPubSub()

	ch := ps.Subscribe(int64(1))

	// Fill the buffer
	for range subscriberBufferSize {
		ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "fill"})
	}

	// This should not block — the notification is dropped
	done := make(chan struct{})
	go func() {
		ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "overflow"})
		close(done)
	}()

	select {
	case <-done:
		// Good — Publish returned without blocking
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}

	// Drain and verify we got exactly bufferSize messages
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			require.Equal(t, subscriberBufferSize, count)
			return
		}
	}
}

func TestPubSub_Unsubscribe(t *testing.T) {
	ps := newPubSub()

	ch := ps.Subscribe(int64(1))
	ps.Unsubscribe(int64(1), ch)

	// Publish should not panic and ch should not receive
	ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "after unsub"})

	select {
	case <-ch:
		t.Fatal("should not receive after unsubscribe")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestPubSub_UnsubscribeAll(t *testing.T) {
	ps := newPubSub()

	ch := ps.SubscribeAll()
	ps.UnsubscribeAll(ch)

	ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "after unsub"})

	select {
	case <-ch:
		t.Fatal("should not receive after unsubscribe")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}

func TestPubSub_PerSessionIsolation(t *testing.T) {
	ps := newPubSub()

	ch1 := ps.Subscribe(int64(1))
	ch2 := ps.Subscribe(int64(2))

	ps.Publish(int64(1), sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "for s1"})

	select {
	case got := <-ch1:
		assert.Equal(t, "for s1", got.Message)
	case <-time.After(time.Second):
		t.Fatal("ch1 timeout")
	}

	select {
	case <-ch2:
		t.Fatal("s2 subscriber should not receive s1 notification")
	case <-time.After(50 * time.Millisecond):
		// Expected
	}
}
