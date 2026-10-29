package assistant

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantToolsRefuseCommands(t *testing.T) {
	for _, id := range []string{toolOpen, toolSend} {
		for _, text := range []string{"/budget 5$", "  /config", "/etc/nginx.conf needs edits"} {
			t.Run(id+text, func(t *testing.T) {
				t.Parallel()
				f := newToolFixture(t)
				args, err := json.Marshal(map[string]any{
					"session": f.work, "project": "devbox", "about": "work", "text": text,
				})
				require.NoError(t, err)
				_, err = f.tools[id].Execute(t.Context(), args)
				require.EqualError(t, err, id+": commands belong to the user; use set_model, set_budget or stop")
				assert.Empty(t, f.host.created)
				assert.Empty(t, f.host.deliveries)
			})
		}
	}
}

func TestAssistantSendHashSession(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	f.execute(t, toolSend, fmt.Sprintf(`{"session":"#%d","text":"continue"}`, f.work))
	assert.Equal(t, []string{"continue"}, f.host.deliveries)
	pending, err := f.sessions.ListPending(t.Context(), f.work)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "continue", pending[0].RawContent)
}
