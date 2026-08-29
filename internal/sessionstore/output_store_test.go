package sessionstore

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutputStore_GlobalFIFOAndAttemptCAS(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	first, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	second, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	require.NoError(
		t,
		store.BindManager(ctx, "telegram", "telegram", map[string]any{
			"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
		}),
	)

	a, err := store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   first.ID,
			Type:        OutputMessagePersistent,
			Content:     "first",
			SourceKey:   "a",
			Fingerprint: OutputFingerprint(OutputMessagePersistent, "first", first.ID, nil),
		},
	)
	require.NoError(t, err)
	b, err := store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   second.ID,
			Type:        OutputMessagePersistent,
			Content:     "second",
			SourceKey:   "b",
			Fingerprint: OutputFingerprint(OutputMessagePersistent, "second", second.ID, nil),
		},
	)
	require.NoError(t, err)
	assert.Less(t, a.OutputID, b.OutputID)

	claim, err := store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	require.Equal(t, a.OutputID, claim.Output.ID)
	require.Equal(t, int64(1), claim.Output.AttemptSeq)

	var persistedAttemptSeq int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT attempt_seq FROM session_outbox WHERE id = ?`, claim.Output.ID).Scan(&persistedAttemptSeq))
	require.Equal(t, int64(1), persistedAttemptSeq)
	_, err = store.ClaimOutputHead(ctx, "telegram")
	require.ErrorIs(t, err, ErrNoOutput, "the claimed global head blocks later sessions")

	require.NoError(t, store.AckOutput(ctx, "telegram", claim.Output.ID, claim.Output.AttemptID, []string{"1"}, nil))

	var attributes string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT attributes FROM session_outbox WHERE id = ?`, claim.Output.ID).Scan(&attributes))
	assert.JSONEq(t, `{"manager_id":"telegram","message_ids":["1"],"model_input_generation":0}`, attributes)

	stale := store.AckOutput(ctx, "telegram", claim.Output.ID, claim.Output.AttemptID, []string{"1"}, nil)
	require.ErrorIs(t, stale, ErrOutputAttempt)

	claim, err = store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.Equal(t, b.OutputID, claim.Output.ID)
	assert.Nil(t, claim.PreviousDeliveredOutput, "receipt chains are session-local")
	require.NoError(t, store.AckOutput(ctx, "telegram", claim.Output.ID, claim.Output.AttemptID, []string{"2"}, nil))
	third, err := store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   first.ID,
			Type:        OutputMessageReplaceable,
			Content:     "third",
			SourceKey:   "c",
			Fingerprint: OutputFingerprint(OutputMessageReplaceable, "third", first.ID, nil),
		},
	)
	require.NoError(t, err)
	claim, err = store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.Equal(t, third.OutputID, claim.Output.ID)
	require.NotNil(t, claim.PreviousDeliveredOutput)
	assert.Equal(t, a.OutputID, claim.PreviousDeliveredOutput.ID)
}

func TestOutputStore_RetryBlockAndOwnership(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	telegram, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	cli, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	require.NoError(t, store.BindManager(ctx, "telegram", "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	}))
	require.NoError(t, store.BindManager(ctx, "cli", "cli", map[string]any{"local": true}))

	entry, err := store.EnqueueOutput(
		ctx,
		OutputDraft{SessionID: telegram.ID, Type: OutputMessageReplaceable, Content: "hello"},
	)
	require.NoError(t, err)
	_, err = store.EnqueueOutput(ctx, OutputDraft{SessionID: cli.ID, Type: OutputMessagePersistent, Content: "local"})
	require.NoError(t, err)
	claim, err := store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	require.NoError(
		t,
		store.RetryOutput(
			ctx,
			"telegram",
			claim.Output.ID,
			claim.Output.AttemptID,
			"temporary",
			time.Now().Add(time.Hour),
		),
	)
	_, err = store.ClaimOutputHead(ctx, "telegram")
	require.ErrorIs(t, err, ErrNoOutput)
	cliClaim, err := store.ClaimOutputHead(ctx, "cli")
	require.NoError(t, err, "one manager's delayed head must not block another")
	require.NoError(t, store.AckOutput(ctx, "cli", cliClaim.Output.ID, cliClaim.Output.AttemptID, []string{}, nil))

	recovered, err := store.RecoverInterruptedOutputs(ctx)
	require.NoError(t, err)
	assert.Zero(t, recovered)
	blocked, err := store.RetryBlockedHead(ctx, "telegram")
	require.NoError(t, err)
	assert.False(t, blocked)

	_, err = store.ClaimOutputHead(ctx, "telegram")
	require.ErrorIs(t, err, ErrNoOutput)
	woken, err := store.WakeOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.True(t, woken)
	claim, err = store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	require.NoError(t, store.BlockOutput(ctx, "telegram", claim.Output.ID, claim.Output.AttemptID, "permission denied"))
	woken, err = store.WakeOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.False(t, woken, "a blocked head stays blocked until manager restart")
	assert.Equal(t, int64(1), entry.OutputID)
}

func TestOutputStore_RejectsIdentityAndOwnershipViolations(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	owned, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)
	ownerless, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(ctx, projectID, owned.ID, owned.ID, "general", "model", "")
	require.NoError(t, err)

	_, err = store.EnqueueOutput(ctx, OutputDraft{SessionID: ownerless.ID, Type: OutputMessagePersistent, Content: "x"})
	require.ErrorIs(t, err, ErrOutputOwner)
	_, err = store.EnqueueOutput(ctx, OutputDraft{SessionID: child, Type: OutputMessagePersistent, Content: "x"})
	require.ErrorIs(t, err, ErrOutputNotRoot)
	_, err = store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   owned.ID,
			Type:        OutputMessagePersistent,
			Content:     "x",
			SourceKey:   "key",
			Fingerprint: OutputFingerprint(OutputMessagePersistent, "x", owned.ID, nil),
		},
	)
	require.NoError(t, err)
	_, err = store.EnqueueOutput(
		ctx,
		OutputDraft{
			SessionID:   owned.ID,
			Type:        OutputMessagePersistent,
			Content:     "changed",
			SourceKey:   "key",
			Fingerprint: OutputFingerprint(OutputMessagePersistent, "changed", owned.ID, nil),
		},
	)
	require.ErrorIs(t, err, ErrOutputConflict)

	require.NoError(t, store.BindManager(ctx, "alpha", "cli", map[string]any{"local": true}))
	err = store.BindManager(ctx, "alpha", "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	})
	require.ErrorIs(t, err, ErrManagerBinding)
	assert.NotErrorIs(t, err, ErrNoOutput)
}

func TestOutputStore_RejectsFingerprintThatDoesNotMatchPayload(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	owned, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)

	fingerprint := OutputFingerprint(OutputMessagePersistent, "original", owned.ID, nil)
	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: owned.ID, Type: OutputMessagePersistent, Content: "original",
		SourceKey: "key", Fingerprint: fingerprint,
	})
	require.NoError(t, err)

	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: owned.ID, Type: OutputMessagePersistent, Content: "changed",
		SourceKey: "key", Fingerprint: fingerprint,
	})
	require.ErrorIs(t, err, ErrOutputConflict)
}

func TestOutputStore_RejectsIncompleteBuiltInManagerBinding(t *testing.T) {
	ctx := context.Background()
	store, _, _ := newTestStore(t)

	err := store.BindManager(ctx, "telegram", "telegram", map[string]any{"bot_user_id": int64(1)})
	require.ErrorIs(t, err, ErrManagerBinding)

	err = store.BindManager(ctx, "cli", "cli", map[string]any{"local": false})
	require.ErrorIs(t, err, ErrManagerBinding)
}

func TestOutputStore_RejectsEachMissingManagerBindingComponent(t *testing.T) {
	ctx := context.Background()
	store, _, _ := newTestStore(t)

	tests := []struct {
		name       string
		managerID  string
		driver     string
		attributes map[string]any
		message    string
	}{
		{
			name:       "manager",
			driver:     "custom",
			attributes: map[string]any{"identity": "x"},
			message:    "manager binding requires manager, driver, and identity",
		},
		{
			name:       "driver",
			managerID:  "custom",
			attributes: map[string]any{"identity": "x"},
			message:    "manager binding requires manager, driver, and identity",
		},
		{
			name:      "identity",
			managerID: "custom",
			driver:    "custom",
			message:   "manager binding requires manager, driver, and identity",
		},
		{
			name:       "unencodable identity",
			managerID:  "custom",
			driver:     "custom",
			attributes: map[string]any{"identity": make(chan struct{})},
			message:    "manager binding requires object identity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.EqualError(t, store.BindManager(ctx, tt.managerID, tt.driver, tt.attributes), tt.message)
		})
	}
}

func TestValidateManagerBinding_RejectsEachTelegramIdentityViolation(t *testing.T) {
	valid := map[string]any{"bot_user_id": int64(1), "chat_id": int64(-2), "topology": "group"}
	require.NoError(t, validateManagerBinding("telegram", valid))
	require.NoError(t, validateManagerBinding("telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "bot",
	}))

	tests := []struct {
		name       string
		attributes map[string]any
	}{
		{
			name: "extra field",
			attributes: map[string]any{
				"bot_user_id": int64(1),
				"chat_id":     int64(2),
				"topology":    "group",
				"extra":       true,
			},
		},
		{name: "bot type", attributes: map[string]any{"bot_user_id": "1", "chat_id": int64(2), "topology": "group"}},
		{name: "chat type", attributes: map[string]any{"bot_user_id": int64(1), "chat_id": "2", "topology": "group"}},
		{
			name:       "topology type",
			attributes: map[string]any{"bot_user_id": int64(1), "chat_id": int64(2), "topology": true},
		},
		{
			name:       "topology value",
			attributes: map[string]any{"bot_user_id": int64(1), "chat_id": int64(2), "topology": "channel"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, validateManagerBinding("telegram", tt.attributes), ErrManagerBinding)
		})
	}
}

func TestValidBindingInt64_AcceptsOnlyNonzeroIntegralNumbers(t *testing.T) {
	for _, value := range []any{int64(-1), int64(1), int(-1), int(1), float64(-1), float64(1)} {
		assert.True(t, validBindingInt64(value))
	}

	for _, value := range []any{int64(0), int(0), float64(0), 1.5, "1"} {
		assert.False(t, validBindingInt64(value))
	}
}

func TestOutputStore_RejectsEachInvalidAttemptResolutionArgument(t *testing.T) {
	store, _, _ := newTestStore(t)
	ctx := context.Background()

	retryTests := []struct {
		name    string
		failure string
		next    time.Time
	}{
		{name: "empty failure", next: time.Now().Add(time.Minute)},
		{name: "oversized failure", failure: string(make([]byte, 513)), next: time.Now().Add(time.Minute)},
		{name: "zero next attempt", failure: "temporary"},
	}
	for _, tt := range retryTests {
		t.Run("retry "+tt.name, func(t *testing.T) {
			require.EqualError(
				t,
				store.RetryOutput(ctx, "manager", 1, "attempt", tt.failure, tt.next),
				"invalid output retry",
			)
		})
	}

	for _, failure := range []string{"", string(make([]byte, 513))} {
		require.EqualError(t, store.BlockOutput(ctx, "manager", 1, "attempt", failure), "invalid output block")
	}
}

func TestOutputStore_AcceptsMaximumLengthAttemptErrors(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	require.NoError(t, store.BindManager(ctx, "cli", "cli", map[string]any{"local": true}))
	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: record.ID, Type: OutputMessagePersistent, Content: "answer",
	})
	require.NoError(t, err)

	claim, err := store.ClaimOutputHead(ctx, "cli")
	require.NoError(t, err)
	failure := string(make([]byte, 512))
	require.NoError(t, store.RetryOutput(
		ctx, "cli", claim.Output.ID, claim.Output.AttemptID, failure, time.Now().UTC(),
	))

	claim, err = store.ClaimOutputHead(ctx, "cli")
	require.NoError(t, err)
	require.NoError(t, store.BlockOutput(ctx, "cli", claim.Output.ID, claim.Output.AttemptID, failure))
}

func TestOutputStore_AssistantMessageAndOutputCommitTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)

	messageID, output, err := store.InsertAssistantMessageWithOutput(ctx, record.ID, &StoredMessage{
		Role: "assistant", Content: "answer",
	}, OutputMessagePersistent, "✅ answer")
	require.NoError(t, err)
	require.NotZero(t, messageID)
	require.NotZero(t, output.OutputID)

	var sessionID int64
	var sourceKey string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT session_id, source_key FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&sessionID, &sourceKey))
	assert.Equal(t, record.ID, sessionID)
	assert.Equal(t, "message:"+strconv.FormatInt(messageID, 10)+":final", sourceKey)
}

func TestOutputStore_HandleInputWithOutputCommitsTheCommandAndAnswerTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/status")
	require.NoError(t, err)

	output, err := store.HandleInputWithOutput(ctx, input.ID, "status command", OutputDraft{
		SessionID: record.ID,
		Type:      OutputMessagePersistent,
		Content:   "## Session Status",
	})
	require.NoError(t, err)
	require.NotZero(t, output.OutputID)

	var state, content, sourceKey string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&state))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT content, source_key FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&content, &sourceKey))
	assert.Equal(t, string(InputStateHandled), state)
	assert.Equal(t, "## Session Status", content)
	assert.Equal(t, "input:"+strconv.FormatInt(input.ID, 10)+":status:result", sourceKey)

	_, err = store.HandleInputWithOutput(ctx, input.ID, "status command", OutputDraft{
		SessionID: record.ID,
		Type:      OutputMessagePersistent,
		Content:   "## Session Status",
	})
	require.ErrorIs(t, err, ErrInputResolved)
}

func TestOutputStore_MarkSessionKilledWithOutputCommitsBoth(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "alpha"})
	require.NoError(t, err)

	output, err := store.MarkSessionKilledWithOutput(ctx, record.ID)
	require.NoError(t, err)
	require.NotNil(t, output)

	var status, outputType string
	var killedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status, killed_at FROM sessions WHERE id = ?`, record.ID).Scan(&status, &killedAt))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT type FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&outputType))
	assert.Equal(t, string(SessionStatusKilled), status)
	assert.False(t, killedAt.IsZero())
	assert.Equal(t, string(OutputSessionClosed), outputType)
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
	input, err := store.EnqueueInput(ctx, old.ID, InputSourceUser, "/clear")
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
		{kind: string(OutputMessagePersistent), content: "Session cleared."},
	}, outputs)
}

func TestOutputStore_LifecycleInputSetsFenceAndAcknowledgementTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/stop")
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
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/kill")
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

	require.NoError(t, store.KillTerminatingSessions(ctx))

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
	_, _, err = store.InsertAssistantMessageWithOutput(ctx, record.ID, &StoredMessage{
		Role: "assistant", Content: "late answer",
	}, OutputMessagePersistent, "✅ late answer")
	require.ErrorContains(t, err, "cannot commit ordinary output")
}

func TestOutputStore_ResolvesManagerOwnedReplacementChain(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "cli"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	newRecord, _, err := store.ReplaceManagerRoot(ctx, old.ID, "project", "/work/project")
	require.NoError(t, err)
	_, err = store.MarkSessionKilledWithOutput(ctx, old.ID)
	require.NoError(t, err)

	resolved, err := store.ResolveReplacement(ctx, old.ID, "cli")
	require.NoError(t, err)
	assert.Equal(t, newRecord.ID, resolved)
}
