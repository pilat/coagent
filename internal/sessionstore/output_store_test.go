package sessionstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestOutputStore_AppendsAndReplaysWithoutMutatingHistory(t *testing.T) {
	store, db, project := newTestStore(t)
	root, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "owner"})
	require.NoError(t, err)
	draft := OutputDraft{SessionID: root.ID, Type: OutputMessage, Content: "first", SourceKey: "first"}
	draft.Fingerprint = OutputFingerprint(draft.Type, draft.Content, draft.SessionID, nil, false)
	first, err := store.EnqueueOutput(t.Context(), draft)
	require.NoError(t, err)
	second, err := store.EnqueueOutput(
		t.Context(),
		OutputDraft{SessionID: root.ID, Type: OutputMessage, Content: "second"},
	)
	require.NoError(t, err)
	store = testStore(db)
	replay, err := store.EnqueueOutput(t.Context(), draft)
	require.NoError(t, err)
	assert.True(t, replay.Existing)
	assert.Equal(t, first.OutputID, replay.OutputID)
	assert.Less(t, first.OutputID, second.OutputID)
	rows, err := store.ChatOutputs(t.Context(), "owner", "chat", root.ID, 100)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "first", rows[0].Content)
	assert.Equal(t, "second", rows[1].Content)
	assert.Equal(t, first.OutputID, rows[0].ID)
}

func TestOutputStore_RejectsIdentityAndOwnershipViolations(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	owned, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)
	ownerless, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	child, err := createChild(ctx, store, projectID, owned.ID, owned.ID, "general", "model", "")
	require.NoError(t, err)

	skipped, err := store.EnqueueOutput(
		ctx,
		OutputDraft{SessionID: ownerless.ID, Type: OutputMessage, Content: "x"},
	)
	require.NoError(t, err)
	assert.Nil(t, skipped)
	skipped, err = store.EnqueueOutput(ctx, OutputDraft{SessionID: child, Type: OutputMessage, Content: "x"})
	require.NoError(t, err)
	assert.Nil(t, skipped)
	_, err = store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   owned.ID,
			Type:        OutputMessage,
			Content:     "x",
			SourceKey:   "key",
			Fingerprint: OutputFingerprint(OutputMessage, "x", owned.ID, nil, false),
		},
	)
	require.NoError(t, err)
	_, err = store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   owned.ID,
			Type:        OutputMessage,
			Content:     "changed",
			SourceKey:   "key",
			Fingerprint: OutputFingerprint(OutputMessage, "changed", owned.ID, nil, false),
		},
	)
	require.ErrorIs(t, err, ErrOutputConflict)
}

func TestOutputStore_RejectsFingerprintThatDoesNotMatchPayload(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	owned, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)

	fingerprint := OutputFingerprint(OutputMessage, "original", owned.ID, nil, false)
	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: owned.ID, Type: OutputMessage, Content: "original",
		SourceKey: "key", Fingerprint: fingerprint,
	})
	require.NoError(t, err)

	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: owned.ID, Type: OutputMessage, Content: "changed",
		SourceKey: "key", Fingerprint: fingerprint,
	})
	require.ErrorIs(t, err, ErrOutputConflict)
}

func TestOutputStore_MarkSessionKilledWithOutputCommitsBoth(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)
	childID, err := createChild(ctx, store, projectID, record.ID, record.ID, "general", "model", "")
	require.NoError(t, err)
	rootInput, err := enqueueFact(ctx, store, record.ID, InputSourceProcess, "root process", nil)
	require.NoError(t, err)
	childInput, err := enqueueFact(ctx, store, childID, InputSourceSubagent, "child completion", nil)
	require.NoError(t, err)

	output, err := store.MarkSessionKilledWithOutput(ctx, record.ID, 3)
	require.NoError(t, err)
	require.NotNil(t, output)

	var status, outputType, content string
	var cancelledProcesses int
	var killedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status, killed_at FROM sessions WHERE id = ?`, record.ID).Scan(&status, &killedAt))
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT type, content, json_extract(attributes, '$.cancelled_processes')
		FROM session_outbox WHERE id = ?`, output.OutputID,
	).Scan(&outputType, &content, &cancelledProcesses))
	assert.Equal(t, string(SessionStatusKilled), status)
	assert.False(t, killedAt.IsZero())
	assert.Equal(t, string(OutputSessionClosed), outputType)
	assert.Equal(t, "Session killed. Cancelled background processes: 3", content)
	assert.Equal(t, 3, cancelledProcesses)
	assertInputState(t, db, rootInput.ID, InputStateCancelled)
	assertInputState(t, db, childInput.ID, InputStateCancelled)
}

func TestOutputStore_CreatesManagerRootWithLifecycleAndInitialInputAtomically(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, output, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Prompt: "start", StartEpisode: true, Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	require.NotZero(t, record.ID)
	require.NotZero(t, output.OutputID)

	var outputType, inputContent, inputOwner string
	var episodeStartedAt time.Time
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT type FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&outputType),
	)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT raw_content, json_extract(attributes, '$.manager_id')
		FROM session_inbox WHERE session_id = ?`, record.ID).Scan(&inputContent, &inputOwner))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT episode_started_at FROM sessions WHERE id = ?`, record.ID).Scan(&episodeStartedAt))
	assert.Equal(t, string(OutputSessionOpened), outputType)
	assert.Equal(t, "start", inputContent)
	assert.Equal(t, "telegram", inputOwner)
	assert.False(t, episodeStartedAt.IsZero())
}

func TestOutputStore_ReplacesManagerRootWithOneLifecycleOutput(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	newRecord, output, err := store.ReplaceManagerRoot(ctx, old.ID, "project", "/work/project")
	require.NoError(t, err)
	require.NotZero(t, output.OutputID)

	var oldStatus, outputType string
	var oldID, newID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, old.ID).Scan(&oldStatus))
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT type, json_extract(attributes, '$.old_session_id'), json_extract(attributes, '$.new_session_id')
		FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&outputType, &oldID, &newID))
	assert.Equal(t, string(SessionStatusTerminating), oldStatus)
	assert.Equal(t, string(OutputSessionReplaced), outputType)
	assert.Equal(t, old.ID, oldID)
	assert.Equal(t, newRecord.ID, newID)
}

func TestOutputStore_ClearInputReplacesRootAndAcknowledgesAtomically(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, old.ID, InputSourceUser, "/clear")
	require.NoError(t, err)
	newRecord, _, err := store.ReplaceManagerRootForInput(ctx, old.ID, input.ID, "project", "/work/project")
	require.NoError(t, err)

	var state string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&state))
	assert.Equal(t, string(InputStateHandled), state)

	rows, err := db.QueryContext(
		ctx,
		`SELECT type, content FROM session_outbox WHERE session_id = ? ORDER BY id`,
		newRecord.ID,
	)
	require.NoError(t, err)
	defer rows.Close()
	var outputs []struct{ kind, content string }
	for rows.Next() {
		var output struct{ kind, content string }
		require.NoError(t, rows.Scan(&output.kind, &output.content))
		outputs = append(outputs, output)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []struct{ kind, content string }{
		{kind: string(OutputSessionReplaced)},
		{kind: string(OutputMessage), content: "Session cleared."},
	}, outputs)
}

func TestOutputStore_LifecycleInputSetsFenceAndAcknowledgementTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, record.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	commit, err := store.BeginLifecycleInput(ctx, input.ID, "stop", "⏹ Stopping...")
	require.NoError(t, err)

	var status, inputState, content string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, record.ID).Scan(&status))
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&inputState),
	)
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT content FROM session_outbox WHERE id = ?`, commit.OutputID).Scan(&content),
	)
	assert.Equal(t, string(SessionStatusStopping), status)
	assert.Equal(t, string(InputStateHandled), inputState)
	assert.Equal(t, "⏹ Stopping...", content)
}

// Kill hands the root to boot reconciliation, so its fence is terminating: a
// crash after the fence selects kill cleanup, not an interrupted stop.
func TestOutputStore_KillInputFencesTerminating(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, record.ID, InputSourceUser, "/kill")
	require.NoError(t, err)

	_, err = store.BeginLifecycleInput(ctx, input.ID, "kill", "Stopping session...")
	require.NoError(t, err)

	var status string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, record.ID).Scan(&status))
	assert.Equal(t, string(SessionStatusTerminating), status)
}

// Boot reconciliation kills a terminating root; without a replacement row it
// must also emit the close output a live Kill would have delivered.
func TestOutputStore_BootReconciliationEmitsCloseWithoutReplacement(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	killed, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, killed.ID, SessionStatusTerminating))

	cleared, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	_, _, err = store.ReplaceManagerRoot(ctx, cleared.ID, "project", "/work/project")
	require.NoError(t, err)

	for _, id := range []int64{killed.ID, cleared.ID} {
		_, err = store.MarkSessionKilledWithOutput(ctx, id, 0)
		require.NoError(t, err)
	}

	for _, id := range []int64{killed.ID, cleared.ID} {
		var status string
		var killedAt *time.Time
		require.NoError(
			t,
			db.QueryRowContext(ctx, `SELECT status, killed_at FROM sessions WHERE id = ?`, id).Scan(&status, &killedAt),
		)
		assert.Equal(t, string(SessionStatusKilled), status)
		require.NotNil(t, killedAt)
	}

	rows, err := db.QueryContext(ctx, `SELECT session_id FROM session_outbox WHERE type = 'session_closed'`)
	require.NoError(t, err)
	defer rows.Close()

	closed := make([]int64, 0)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		closed = append(closed, id)
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{killed.ID}, closed,
		"the mid-kill root closes its surface; the mid-clear root transferred it to its replacement")
}

func TestOutputStore_RejectsAssistantOutputAfterLifecycleFence(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusStopping))
	_, _, err = appendPublished(ctx, store, record.ID, &transcript.Message{
		Role: "assistant", Content: "late answer",
	}, OutputMessage, "✅ late answer", true)
	require.ErrorIs(t, err, ErrSessionStopping)
}

func TestOutputStore_ResolvesManagerOwnedReplacementChain(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram-test"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	newRecord, _, err := store.ReplaceManagerRoot(ctx, old.ID, "project", "/work/project")
	require.NoError(t, err)
	_, err = store.MarkSessionKilledWithOutput(ctx, old.ID, 0)
	require.NoError(t, err)

	resolved, err := store.ResolveReplacement(ctx, old.ID, "telegram-test")
	require.NoError(t, err)
	assert.Equal(t, newRecord.ID, resolved)
}
