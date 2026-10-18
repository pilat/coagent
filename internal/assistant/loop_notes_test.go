package assistant

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantLoopRepeatedNotesKeepAge(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name, text := "unchanged", "Russian"
		if changed {
			name, text = "changed", "English"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer := `{"say":"done","notes":[{"key":"user.lang","text":"` + text + `"}]}`
			responses := []string{answer}
			if changed {
				responses = append(responses, sayAnswer("corrected"))
			}
			f := newLoopFixture(t, responses...)
			before := time.Unix(1000, 0)
			require.NoError(t, f.store.WithTx(t.Context(), func(tx *sql.Tx) error {
				return SaveNotesTx(t.Context(), tx, f.root, map[string]*string{"user.lang": new("Russian")}, before)
			}))
			require.NoError(t, f.deps.Enqueue(t.Context(), "answered", 244, "ready"))
			require.NoError(t, Run(t.Context(), f.deps, f.root))
			desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
			require.NoError(t, err)
			assert.Equal(
				t,
				Note{Key: "user.lang", Text: "Russian", WrittenAt: before.Unix()},
				noteMap(desk.Notes)["user.lang"],
			)
			if changed {
				assert.Contains(t, f.client.desks[1], "starts with the user's message")
				assert.Equal(t, []string{"corrected"}, f.notified)
			} else {
				assert.Equal(t, []string{"done"}, f.notified)
			}
		})
	}
}

func TestAssistantLoopRepeatedOpenNotesStillBind(t *testing.T) {
	t.Parallel()
	f := newLoopFixture(
		t,
		`{"do":[{"open":{"project":"new","about":"purpose","text":"go"}}],"notes":[{"key":"wait.new","text":"retell"},{"key":"project.new","text":"state"}]}`,
	)
	require.NoError(t, f.store.WithTx(t.Context(), func(tx *sql.Tx) error {
		return SaveNotesTx(t.Context(), tx, f.root, map[string]*string{
			"wait.new": new("retell"), "project.new": new("state"),
		}, time.Unix(1000, 0))
	}))
	f.enqueue("open")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
	require.NoError(t, err)
	notes := noteMap(desk.Notes)
	assert.Equal(t, []string{"open new go"}, f.sessions.moves)
	assert.NotContains(t, notes, "wait.new")
	assert.Equal(t, "retell", notes["wait.245"].Text)
	assert.Greater(t, notes["wait.245"].WrittenAt, int64(1000))
	assert.Equal(t, "state", notes["project.new"].Text)
	assert.Greater(t, notes["project.new"].WrittenAt, int64(1000))
}

func TestAssistantLoopRejectsSendWithoutWait(t *testing.T) {
	f := newLoopFixture(t, `{"do":[{"send":{"session":244,"text":"go"}}]}`, sayAnswer("corrected"))
	f.enqueue("send")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Empty(t, f.sessions.moves)
	assert.Contains(t, f.client.desks[1], "every open and send needs a wait note")
}

func TestAssistantLoopPinnedNotesFollowOriginAcrossReads(t *testing.T) {
	for _, origin := range []string{"user", "answered"} {
		t.Run(origin, func(t *testing.T) {
			responses := []string{
				actionAnswer(`{"read":{"session":244,"lines":1}}`),
				`{"notes":[{"key":"user.lang","text":"Russian"}]}`,
			}
			if origin != "user" {
				responses = append(responses, sayAnswer("corrected"))
			}
			f := newLoopFixture(t, responses...)
			if origin == "user" {
				f.enqueue("remember")
			} else {
				require.NoError(t, f.deps.Enqueue(t.Context(), "answered", 244, "← devbox (244) answered: ready"))
			}
			require.NoError(t, Run(t.Context(), f.deps, f.root))
			desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
			require.NoError(t, err)
			_, exists := noteMap(desk.Notes)["user.lang"]
			assert.Equal(t, origin == "user", exists)
			if origin != "user" {
				assert.Contains(t, f.client.desks[2], "starts with the user's message")
			}
		})
	}
}

func TestAssistantLoopFailedOpenDiscardsOnlyRawNotes(t *testing.T) {
	for _, project := range []string{"new", "devbox"} {
		t.Run(project, func(t *testing.T) {
			answer := `{"do":[{"open":{"project":"` + project + `","text":"go","about":"why"}}],"notes":[{"key":"wait.` + project + `","text":"retell"},{"key":"project.` + project + `","text":"state"}]}`
			f := newLoopFixture(t, answer, sayAnswer("open failed"))
			f.sessions.err = errors.New("unavailable")
			f.enqueue("open")
			require.NoError(t, Run(t.Context(), f.deps, f.root))
			desk, err := f.deps.Memory.LoadDesk(t.Context(), f.root)
			require.NoError(t, err)
			notes := noteMap(desk.Notes)
			assert.NotContains(t, notes, "wait."+project)
			if project == "new" {
				assert.Empty(t, notes)
			} else {
				assert.Equal(t, "state", notes["project.devbox"].Text)
			}
		})
	}
}

func TestAssistantLoopRekeyFailureDoesNotReportOpenFailure(t *testing.T) {
	f := newLoopFixture(
		t,
		`{"do":[{"open":{"project":"devbox","text":"go","about":"why"}}],"notes":[{"key":"wait.devbox","text":"retell"}]}`,
		sayAnswer("next"),
	)
	_, err := f.db.ExecContext(t.Context(), `CREATE TRIGGER refuse_rekey BEFORE INSERT ON assistant_notes
		WHEN NEW.key = 'wait.245' BEGIN SELECT RAISE(ABORT,'rename unavailable'); END`)
	require.NoError(t, err)
	f.enqueue("open")
	f.enqueue("next")
	require.NoError(t, Run(t.Context(), f.deps, f.root))
	assert.Equal(t, []string{"open devbox go"}, f.sessions.moves)
	assert.Contains(t, f.client.desks[1], "## orphaned\nwait.devbox")
	var count int
	require.NoError(
		t,
		f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox WHERE source='agent'`).Scan(&count),
	)
	assert.Zero(t, count)
}
