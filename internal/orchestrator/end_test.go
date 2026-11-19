package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndToolsResults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		id     string
		ended  Ended
		output string
	}{
		{id: IDStopSession, ended: Ended{ID: 25, Running: true, Changed: true}, output: "Stopped session #25; its running turn was cancelled."},
		{id: IDStopSession, ended: Ended{ID: 25, Changed: true}, output: "Stopped session #25; it had no running turn, and its queued and background work was cancelled."},
		{id: IDKillSession, ended: Ended{ID: 25, Changed: true}, output: "Killed session #25."},
		{id: IDKillSession, ended: Ended{ID: 25}, output: "Session #25 was already killed."},
	} {
		t.Run(tc.output, func(t *testing.T) {
			host := &fakeHost{ended: tc.ended}
			result, err := findTool(t, host, tc.id).Execute(t.Context(), json.RawMessage(`{"session_id":13}`))
			require.NoError(t, err)
			assert.Equal(t, tc.output, result.Output)
			title := "stop #25"
			if tc.id == IDKillSession {
				title = "kill #25"
			}
			assert.Equal(t, title, result.Title)
			assert.Equal(t, []hostCall{{caller: 12, target: 13}}, host.calls)
		})
	}

	for _, state := range []string{"idle", "parked", "error", "stopped", "stopping", "terminating"} {
		host := &fakeHost{ended: Ended{ID: 25, State: state}}
		result, err := findTool(t, host, IDStopSession).Execute(t.Context(), json.RawMessage(`{"session_id":13}`))
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("Session #25 is %s; nothing to stop.", state), result.Output)
		assert.Equal(t, "stop #25", result.Title)
	}

	for _, id := range []string{IDStopSession, IDKillSession} {
		host := &fakeHost{err: errors.New("refused")}
		candidate := findTool(t, host, id)
		result, err := candidate.Execute(t.Context(), json.RawMessage(`{"session_id":13}`))
		require.ErrorIs(t, err, host.err)
		assert.Nil(t, result)
		for _, params := range []string{`{}`, `{"session_id":0}`, `{"session_id":-1}`} {
			host.calls = nil
			result, err = candidate.Execute(t.Context(), json.RawMessage(params))
			require.EqualError(t, err, "session_id is required")
			assert.Nil(t, result)
			assert.Empty(t, host.calls)
		}
	}
}

func TestEndToolsTakeOnlySessionID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{IDStopSession, IDKillSession} {
		assert.JSONEq(t, watchParameters, string(findTool(t, &fakeHost{}, id).Parameters()))
	}
}
