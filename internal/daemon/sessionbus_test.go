package daemon

import (
	"testing"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
)

func requireManagerNotification(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
) controllerapi.SessionNotification {
	t.Helper()

	select {
	case notification := <-ch:
		return notification
	case <-time.After(time.Second):
		t.Fatal("manager notification timeout")
	}

	return controllerapi.SessionNotification{}
}

func requireNoManagerNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) {
	t.Helper()

	select {
	case notification := <-ch:
		t.Fatalf("unexpected manager notification: %#v", notification)
	case <-time.After(20 * time.Millisecond):
	}
}
