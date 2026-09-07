package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxStore_PromoteWithReceiptCommitsOnePersistentOutput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/skill review")
	require.NoError(t, err)

	message, commit, err := store.PromoteInputWithReceipt(ctx, input.ID, "[stamp] /skill review", OutputDraft{
		Type:    OutputMessagePersistent,
		Content: "🔧 Activated skill: review",
	})
	require.NoError(t, err)
	require.NotNil(t, message)
	require.NotNil(t, commit)
	require.NotZero(t, commit.OutputID)

	var outputType, content, owner string
	var generation int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT type, content, json_extract(attributes, '$.manager_id'),
			json_extract(attributes, '$.model_input_generation')
		FROM session_outbox WHERE id = ?`, commit.OutputID,
	).Scan(&outputType, &content, &owner, &generation))
	assert.Equal(t, string(OutputMessagePersistent), outputType)
	assert.Equal(t, "🔧 Activated skill: review", content)
	assert.Equal(t, "telegram", owner)
	assert.Equal(t, int64(1), generation, "the receipt snapshots the advanced generation")

	var state string
	var acceptedMessageID int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state, accepted_message_id FROM session_inbox WHERE id = ?`, input.ID,
	).Scan(&state, &acceptedMessageID))
	assert.Equal(t, "accepted", state)
	assert.Equal(t, message.ID, acceptedMessageID)

	var outboxCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox
		 WHERE session_id = ? AND type = 'message_persistent'`, record.ID,
	).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount)
}

func TestInboxStore_PromoteWithReceiptIsIdempotentOnReplay(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/skill review")
	require.NoError(t, err)

	_, commit, err := store.PromoteInputWithReceipt(ctx, input.ID, "[stamp] /skill review", OutputDraft{
		Type:    OutputMessagePersistent,
		Content: "🔧 Activated skill: review",
	})
	require.NoError(t, err)

	message, replayed, err := store.PromoteInputWithReceipt(ctx, input.ID, "ignored retry", OutputDraft{
		Type:    OutputMessagePersistent,
		Content: "🔧 Activated skill: review",
	})
	require.NoError(t, err)
	require.NotNil(t, message)
	assert.Nil(t, replayed, "an accepted replay resolves no second receipt")

	var outboxCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox
		 WHERE session_id = ? AND type = 'message_persistent'`, record.ID,
	).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount)
	assert.NotZero(t, commit.OutputID)
}

func TestInboxStore_PromoteWithReceiptSkipsOwnerlessAndChildSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	ownerless, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	ownerlessInput, err := store.EnqueueInput(ctx, ownerless.ID, InputSourceUser, "/skill review")
	require.NoError(t, err)

	root, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "telegram"},
		Name: "project", WorkDir: "/work/project",
	})
	require.NoError(t, err)
	childID, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "build", "model", "")
	require.NoError(t, err)
	childInput, err := store.EnqueueInput(ctx, childID, InputSourceAgent, "/skill review")
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		input int64
	}{
		{name: "ownerless root", input: ownerlessInput.ID},
		{name: "subagent input", input: childInput.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, commit, err := store.PromoteInputWithReceipt(ctx, tc.input, "content", OutputDraft{
				Type:    OutputMessagePersistent,
				Content: "🔧 Activated skill: review",
			})
			require.NoError(t, err)
			require.NotNil(t, message)
			assert.Nil(t, commit, "no manager owner, no receipt")
		})
	}
}

func TestInboxStore_PromoteWithReceiptRejectsWrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/skill review")
	require.NoError(t, err)

	_, _, err = store.PromoteInputWithReceipt(ctx, input.ID, "content", OutputDraft{
		Type:    OutputMessageReplaceable,
		Content: "🔧 Activated skill: review",
	})
	require.ErrorContains(t, err, "invalid promotion receipt type")
}
