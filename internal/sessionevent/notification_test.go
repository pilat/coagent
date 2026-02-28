package sessionevent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNotificationValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		n       Notification
		wantErr string
	}{
		{name: "message", n: Notification{Type: NotifyMessage, Message: "hello"}},
		{name: "message missing payload", n: Notification{Type: NotifyMessage}, wantErr: "requires message"},
		{name: "heartbeat", n: Notification{Type: NotifyHeartbeat}},
		{
			name:    "heartbeat with foreign payload",
			n:       Notification{Type: NotifyHeartbeat, Status: "idle"},
			wantErr: "unexpected status",
		},
		{name: "state", n: Notification{Type: NotifyStateChanged, Status: "idle", Reason: "done"}},
		{name: "state missing status", n: Notification{Type: NotifyStateChanged}, wantErr: "requires status"},
		{
			name:    "state from persisted vocabulary",
			n:       Notification{Type: NotifyStateChanged, Status: State("completed")},
			wantErr: "requires status",
		},
		{name: "input", n: Notification{Type: NotifyInputReceived, Message: "go", Source: "scheduler"}},
		{
			name:    "input with unknown source",
			n:       Notification{Type: NotifyInputReceived, Message: "go", Source: "cron"},
			wantErr: "requires source",
		},
		{
			name: "created",
			n:    Notification{Type: NotifySessionCreated, WorkDir: "/tmp/work", Attributes: map[string]any{}},
		},
		{name: "created missing workdir", n: Notification{Type: NotifySessionCreated}, wantErr: "requires work_dir"},
		{
			name: "cleared",
			n:    Notification{Type: NotifySessionCleared, WorkDir: "/tmp/work", OldSessionID: 1, NewSessionID: 2},
		},
		{
			name:    "cleared missing old",
			n:       Notification{Type: NotifySessionCleared, WorkDir: "/tmp/work", NewSessionID: 2},
			wantErr: "old_session_id",
		},
		{
			name:    "cleared missing new",
			n:       Notification{Type: NotifySessionCleared, WorkDir: "/tmp/work", OldSessionID: 1},
			wantErr: "new_session_id",
		},
		{
			name:    "cleared same ids",
			n:       Notification{Type: NotifySessionCleared, WorkDir: "/tmp/work", OldSessionID: 2, NewSessionID: 2},
			wantErr: "distinct session IDs",
		},
		{
			name: "secret",
			n:    Notification{Type: NotifySecretRequest, RequestID: "r1", SecretName: "TOKEN", Message: "why"},
		},
		{
			name:    "secret missing correlation",
			n:       Notification{Type: NotifySecretRequest, SecretName: "TOKEN"},
			wantErr: "request_id",
		},
		{
			name: "secret resolved",
			n:    Notification{Type: NotifySecretResolved, RequestID: "r1", SecretName: "TOKEN"},
		},
		{
			name:    "secret resolved missing correlation",
			n:       Notification{Type: NotifySecretResolved, SecretName: "TOKEN"},
			wantErr: "request_id",
		},
		{
			name:    "secret resolved carrying prose",
			n:       Notification{Type: NotifySecretResolved, RequestID: "r1", SecretName: "TOKEN", Message: "why"},
			wantErr: "unexpected message",
		},
		{name: "unknown", n: Notification{Type: "surprise"}, wantErr: "unknown notification type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.n.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}

			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestFormatWaitingDoesNotPromiseSubagentInterruption(t *testing.T) {
	t.Parallel()

	message := FormatWaiting([]WaitItem{{Kind: WaitSubagent, ChildID: 42}})
	assert.Contains(t, message, "Waiting for subagent #42")
	assert.Contains(t, message, "will be queued")
	assert.NotContains(t, message, "interrupt")
}
