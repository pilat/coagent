package assistant

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantForwardExactCurrentInput(t *testing.T) {
	f := newToolFixture(t)
	words := " \n**yes**, do it\r\n\n> unchanged  "
	acceptForwardInput(t, f, sessionstore.InputSourceUser, words, true)
	result, err := f.tools["forward"].Execute(
		t.Context(),
		json.RawMessage(fmt.Sprintf(`{"session":"#%d","text":"invented replacement"}`, f.work)),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.AnswersInput)
	assert.Equal(t, fmt.Sprintf("forwarded the user's message to session %d", f.work), result.Output)
	pending, err := f.sessions.ListPending(t.Context(), f.work)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, words, pending[0].RawContent)
	assert.True(t, sessionstore.IsHumanInput(pending[0].Source, pending[0].Attributes))
	assert.NotContains(t, pending[0].Attributes, "from")
	assert.EqualValues(t, f.root, pending[0].Attributes[sessionstore.ObserverSessionIDAttribute])
	assert.Equal(t, []string{words}, f.host.deliveries)
}

func TestAssistantForwardRefusalsWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		source           sessionstore.InputSource
		previous         bool
	}{
		{name: "event only", text: "event", source: sessionstore.InputSourceAgent, want: sessionstore.ErrForwardInput.Error()},
		{name: "slash refused", text: "/budget 5", source: sessionstore.InputSourceUser, want: sessionstore.ErrForwardedCommand.Error()},
		{name: "previous run", text: "event", source: sessionstore.InputSourceAgent, previous: true, want: sessionstore.ErrForwardInput.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newToolFixture(t)
			if tc.previous {
				acceptForwardInput(t, f, sessionstore.InputSourceUser, "previous human", true)
			}
			acceptForwardInput(t, f, tc.source, tc.text, true)
			result, err := f.tools["forward"].Execute(
				t.Context(),
				json.RawMessage(fmt.Sprintf(`{"session":%d}`, f.work)),
			)
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, result)
			pending, err := f.sessions.ListPending(t.Context(), f.work)
			require.NoError(t, err)
			assert.Empty(t, pending)
			assert.Empty(t, f.host.deliveries)
			var receipts int
			require.NoError(
				t,
				f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, f.root).
					Scan(&receipts),
			)
			assert.Zero(t, receipts)
		})
	}
}

func acceptForwardInput(t *testing.T, f *toolFixture, source sessionstore.InputSource, words string, begin bool) {
	t.Helper()
	in, err := f.sessions.Enqueue(t.Context(), sessionstore.Input{SessionID: f.root, Source: source, Content: words})
	require.NoError(t, err)
	_, err = f.sessions.Commit(t.Context(), sessionstore.Commit{
		SessionID: f.root,
		BeginRun:  begin,
		Accept: []sessionstore.Accept{
			{
				InputID:    in.Input.ID,
				State:      sessionstore.InputStateAccepted,
				Content:    words,
				LinkRef:    -1,
				ModelBound: true,
			},
		},
	})
	require.NoError(t, err)
}
