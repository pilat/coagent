package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantToolsHandoverOnlyTheirText(t *testing.T) {
	for _, id := range []string{"open", "send"} {
		t.Run(id, func(t *testing.T) {
			f := newToolFixture(t)
			acceptForwardInput(t, f, sessionstore.InputSourceUser, "the user's exact words", true)
			input, err := f.sessions.ActiveHumanInput(t.Context(), f.root)
			require.NoError(t, err)
			require.NotNil(t, input)
			instruction := " \nself-contained instruction\n "
			args, err := json.Marshal(
				map[string]any{"session": f.work, "project": "devbox", "about": "work", "text": instruction},
			)
			require.NoError(t, err)
			f.execute(t, id, string(args))
			text := strings.TrimSpace(instruction)
			assert.Equal(t, []string{text}, f.host.deliveries)
			work := f.work
			if id == "open" {
				work = f.host.created[0]
			}
			pending, err := f.sessions.ListPending(t.Context(), work)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, text, pending[0].RawContent)
		})
	}
}
