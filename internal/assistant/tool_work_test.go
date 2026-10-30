package assistant

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantToolsDeliverCommandsVerbatim(t *testing.T) {
	for _, id := range []string{"open", "send"} {
		for _, text := range []string{"/skill review the diff", "/skill review x", "/budget 5$", "/config", "/etc/nginx.conf needs edits"} {
			t.Run(id+text, func(t *testing.T) {
				t.Parallel()
				f := newToolFixture(t)
				args, err := json.Marshal(map[string]any{
					"session": f.work, "project": "devbox", "about": "work", "text": text,
				})
				require.NoError(t, err)
				_, err = f.tools[id].Execute(t.Context(), args)
				require.NoError(t, err)
				assert.Equal(t, []string{text}, f.host.deliveries)
				work := f.work
				if id == "open" {
					require.Len(t, f.host.created, 1)
					work = f.host.created[0]
				}
				pending, err := f.sessions.ListPending(t.Context(), work)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				assert.Equal(t, text, pending[0].RawContent)
				assert.Equal(t, "assistant", pending[0].Attributes["from"])
				assert.EqualValues(t, f.root, pending[0].Attributes[sessionstore.ObserverSessionIDAttribute])
			})
		}
	}
}

func TestAssistantSendPreservesCommandSnapshotAndProvenance(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	f.host.attributes = map[string]any{
		"schedules":                             "current schedules",
		"from":                                  "user",
		sessionstore.ObserverSessionIDAttribute: int64(-1),
	}
	f.execute(t, "send", fmt.Sprintf(`{"session":%d,"text":"/schedules"}`, f.work))
	pending, err := f.sessions.ListPending(t.Context(), f.work)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "current schedules", pending[0].Attributes["schedules"])
	assert.Equal(t, "assistant", pending[0].Attributes["from"])
	assert.EqualValues(t, f.root, pending[0].Attributes[sessionstore.ObserverSessionIDAttribute])
	assert.False(t, sessionstore.IsHumanInput(pending[0].Source, pending[0].Attributes))
}

func TestAssistantSendHashSession(t *testing.T) {
	t.Parallel()
	f := newToolFixture(t)
	f.execute(t, "send", fmt.Sprintf(`{"session":"#%d","text":"continue"}`, f.work))
	assert.Equal(t, []string{"continue"}, f.host.deliveries)
	pending, err := f.sessions.ListPending(t.Context(), f.work)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "continue", pending[0].RawContent)
}
