package sessionstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

func TestSessionInboxSchema_ResolutionTruthTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	messageID, err := store.InsertMessage(ctx, rec.ID, &transcript.Message{Role: "user", Content: "accepted"})
	require.NoError(t, err)
	now := time.Now().UTC()

	tests := []struct {
		name      string
		source    string
		content   string
		state     string
		resolved  any
		reason    any
		messageID any
		wantErr   bool
	}{
		{name: "pending", source: "user", content: "x", state: "pending"},
		{name: "accepted", source: "agent", content: "x", state: "accepted", resolved: now, messageID: messageID},
		{name: "handled", source: "user", content: "/status", state: "handled", resolved: now, reason: "status"},
		{name: "rejected", source: "user", content: "x", state: "rejected", resolved: now, reason: "bad"},
		{name: "cancelled", source: "agent", content: "x", state: "cancelled", resolved: now, reason: "stop"},
		{name: "bad source", source: "system", content: "x", state: "pending", wantErr: true},
		{name: "empty content", source: "user", content: "", state: "pending", wantErr: true},
		{name: "pending resolved", source: "user", content: "x", state: "pending", resolved: now, wantErr: true},
		{name: "accepted no message", source: "user", content: "x", state: "accepted", resolved: now, wantErr: true},
		{
			name: "accepted reason", source: "user", content: "x", state: "accepted",
			resolved: now, reason: "no", messageID: messageID, wantErr: true,
		},
		{name: "rejected no reason", source: "user", content: "x", state: "rejected", resolved: now, wantErr: true},
		{name: "handled no reason", source: "user", content: "x", state: "handled", resolved: now, wantErr: true},
		{
			name: "cancelled has message", source: "user", content: "x", state: "cancelled",
			resolved: now, reason: "stop", messageID: messageID, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `
				INSERT INTO session_inbox
					(session_id, source, raw_content, received_at, state, resolved_at, resolution_reason, accepted_message_id)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				rec.ID, tt.source, tt.content, now, tt.state, tt.resolved, tt.reason, tt.messageID,
			)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestSessionInboxSchema_PendingIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	_, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	var indexSQL sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_session_inbox_pending_fifo'`,
	).Scan(&indexSQL))
	assert.Contains(t, indexSQL.String, "session_id, id")
	assert.Contains(t, indexSQL.String, "state = 'pending'")
}

func TestSessionInboxSchema_AcceptedIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, db, _ := newTestStore(t)

	var indexSQL sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_session_inbox_accepted_session'`,
	).Scan(&indexSQL))
	assert.Contains(t, indexSQL.String, "session_id, id")
	assert.Contains(t, indexSQL.String, "state = 'accepted'")
	assert.Contains(t, indexSQL.String, "accepted_message_id IS NOT NULL")
}

func TestSessionOutboxSchema_ManagerHeadIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: record.ID, Type: OutputMessagePersistent, Content: "queued",
	})
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN
		SELECT id FROM session_outbox
		WHERE json_extract(attributes, '$.manager_id') = ? AND state <> 'delivered'
		ORDER BY id LIMIT 1`, "telegram")
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, ignored int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &ignored, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, details, "SEARCH session_outbox USING INDEX idx_session_outbox_manager_head (<expr>=?)")
}

func TestSubagentLinkSchema_ActivationSequenceStartsAtOne(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	parent, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	childID, err := subagent.NewTransactions(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "model", TaskCallID: "task-1", State: "spawned",
	})
	require.NoError(t, err)

	var activationSeq int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT activation_seq FROM subagent_links WHERE child_id = ?`, childID,
	).Scan(&activationSeq))
	assert.Equal(t, int64(1), activationSeq)
}

func TestSessionDeliveriesSchema_RejectsInvalidAndDuplicateIdentities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	now := time.Now().UTC()

	_, err = db.ExecContext(ctx, `
		INSERT INTO session_deliveries
			(session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (?, 'd1', 'tool_notification', 'fp', ?)`, rec.ID, now)
	require.NoError(t, err)

	tests := []struct {
		name        string
		sessionID   int64
		deliveryID  string
		kind        string
		fingerprint string
	}{
		{name: "duplicate identity", sessionID: rec.ID, deliveryID: "d1", kind: "tool_notification", fingerprint: "fp"},
		{name: "empty identity", sessionID: rec.ID, kind: "tool_notification", fingerprint: "fp"},
		{name: "unknown kind", sessionID: rec.ID, deliveryID: "d2", kind: "message", fingerprint: "fp"},
		{name: "empty fingerprint", sessionID: rec.ID, deliveryID: "d3", kind: "context_reset"},
		{name: "missing session", sessionID: 999_999, deliveryID: "d4", kind: "context_reset", fingerprint: "fp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, insertErr := db.ExecContext(ctx, `
				INSERT INTO session_deliveries
					(session_id, delivery_id, kind, fingerprint, delivered_at)
				VALUES (?, ?, ?, ?, ?)`,
				tt.sessionID, tt.deliveryID, tt.kind, tt.fingerprint, now,
			)
			require.Error(t, insertErr)
		})
	}
}
