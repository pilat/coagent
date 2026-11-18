package orchestrator

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnreadSignalAcknowledgements(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		id, title, prefix string
	}{
		{id: IDSendToSession, title: "sent to #25", prefix: "Message queued for session #25. "},
		{id: IDSubscribe, title: "subscribed #25", prefix: "Subscribed to session #25. "},
	} {
		t.Run(tc.id, func(t *testing.T) {
			host := &fakeHost{unread: true}
			result, err := findTool(t, host, tc.id).Execute(t.Context(),
				json.RawMessage(`{"session_id":13,"message":"follow-up"}`))
			require.NoError(t, err)
			assert.Equal(t, tc.title, result.Title)
			assert.Equal(t, tc.prefix+"A signal for session #25 is still unread. "+
				"Read it with read_session (session_id 25) before another signal can come.", result.Output)
		})
	}
}
