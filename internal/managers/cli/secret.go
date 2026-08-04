package cli

import (
	"context"
	"encoding/json"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

// OpChatSecretCancel declines a masked prompt. It is a chat op rather than a
// valueless set_secret: nothing is stored, the session is only unblocked.
const OpChatSecretCancel = "chat_secret_cancel"

// SecretCancelParams declines one masked prompt. The request id is what the
// daemon resolves against; the session id keeps the chat ops uniform.
type SecretCancelParams struct {
	SessionID int64  `json:"session_id"`
	RequestID string `json:"request_id"`
}

// SecretRequests is the daemon-side lifecycle of a masked prompt. The prompt is
// pushed once, so a terminal that attached later has no other way to learn it.
type SecretRequests interface {
	PendingSecretRequests(sessionID int64) []sessionevent.Notification
	CancelSecretRequest(ctx context.Context, requestID string) error
}

func (m *Manager) cancelSecretHandler() ctl.Handler {
	return func(ctx context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p SecretCancelParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		if err := m.secrets.CancelSecretRequest(ctx, p.RequestID); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		return SendResult{SessionID: p.SessionID}, nil
	}
}

// replaySecretRequests re-opens the masked prompts this session still waits on: a
// terminal attaching after the push sees nothing while its messages queue.
func (m *Manager) replaySecretRequests(t *terminal, sessionID int64) {
	if sessionID == 0 {
		return
	}

	for _, n := range m.secrets.PendingSecretRequests(sessionID) {
		method, payload := render(controllerapi.SessionNotification{SessionID: sessionID, Notification: n})
		m.queue(t, push{method: method, params: payload})
	}
}
