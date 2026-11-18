package orchestrator

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestSubscribeTools(t *testing.T) {
	t.Parallel()

	t.Run("validation and declarations", testSubscribeDeclarations)
	t.Run("results and errors", testSubscribeResults)
}

func TestRenderSessionsWatchMarkers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{
			ID: 12, Name: "coagent", State: "idle", Activity: now, Self: true,
			Subscribed: true, Armed: true, Signalled: true,
		},
		{ID: 13, Name: "api", State: "idle", Activity: now, Armed: true},
		{ID: 14, Name: "docs", State: "idle", Activity: now, Subscribed: true, Signalled: true},
	}
	assert.Equal(t, "#12 coagent — idle, active 0m ago; subscribed, awaiting signal, unread signal (this session)\n"+
		"#13 api — idle, active 0m ago; awaiting signal\n"+
		"#14 docs — idle, active 0m ago; subscribed, unread signal", renderSessions(entries, 3, "", now))
}

func testSubscribeDeclarations(t *testing.T) {
	t.Helper()

	for index, id := range []string{IDSubscribe, IDUnsubscribe} {
		host := &fakeHost{}
		candidate := findTool(t, host, id)
		for _, args := range []string{`{}`, `{"session_id":0}`, `{"session_id":-1}`} {
			result, err := candidate.Execute(t.Context(), json.RawMessage(args))
			require.EqualError(t, err, "session_id is required")
			assert.Nil(t, result)
			assert.Empty(t, host.calls)
		}

		assert.False(t, candidate.ParallelSafe())
		assert.Equal(t, tool.TrailArgument{}, candidate.(tool.TrailDeclarer).TrailArgument())
		declaration := candidate.(tool.PromptDeclarer).PromptDeclaration()
		assert.Equal(t, tool.PromptGroupSessions, declaration.Group)
		assert.Equal(t, index+5, declaration.Order)
		_, direct := candidate.(tool.DirectOnly)
		assert.False(t, direct)

		var schema map[string]any

		require.NoError(t, json.Unmarshal(candidate.Parameters(), &schema))
		assert.Equal(t, []any{"session_id"}, schema["required"])
	}
}

func testSubscribeResults(t *testing.T) {
	t.Helper()

	for _, tc := range []struct {
		id, title, output string
		was               bool
	}{
		{
			id: IDSubscribe, title: "subscribed #25",
			output: "Subscribed to session #25. You get one signal per turn outcome, and none again until you read it with read_session.",
		},
		{id: IDUnsubscribe, title: "unsubscribed #25", was: true, output: "Unsubscribed from session #25."},
		{id: IDUnsubscribe, title: "unsubscribed #25", output: "You were not subscribed to session #25."},
	} {
		host := &fakeHost{was: tc.was}
		candidate := findTool(t, host, tc.id)
		result, err := candidate.Execute(t.Context(), json.RawMessage(`{"session_id":13}`))
		require.NoError(t, err)
		assert.Equal(t, tc.title, result.Title)
		assert.Equal(t, tc.output, result.Output)
		assert.Equal(t, []hostCall{{caller: 12, target: 13}}, host.calls)
		host.err = errors.New("owner-specific refusal")
		result, err = candidate.Execute(t.Context(), json.RawMessage(`{"session_id":13}`))
		require.ErrorIs(t, err, host.err)
		require.EqualError(t, err, host.err.Error())
		assert.Nil(t, result)
	}
}
