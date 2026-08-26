package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
)

func managerOutboxBackfillMigration() *goose.Migration {
	return goose.NewGoMigration(25, &goose.GoFunc{RunDB: backfillManagerOutbox}, nil)
}

func backfillManagerOutbox(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin manager outbox backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT sessions.id, sessions.attributes, projects.name, projects.work_dir
		FROM sessions JOIN projects ON projects.id = sessions.project_id
		WHERE sessions.parent_id = 0 AND sessions.killed_at IS NULL
			AND sessions.status NOT IN ('terminating', 'killed')
		ORDER BY sessions.updated_at, sessions.id`)
	if err != nil {
		return fmt.Errorf("list manager-owned sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64

		var encoded, name, workDir string
		if err := rows.Scan(&id, &encoded, &name, &workDir); err != nil {
			return fmt.Errorf("scan manager-owned session: %w", err)
		}

		var sessionAttrs map[string]any
		if err := json.Unmarshal([]byte(encoded), &sessionAttrs); err != nil {
			return fmt.Errorf("decode session %d attributes: %w", id, err)
		}

		if sessionAttrs == nil {
			return fmt.Errorf("decode session %d attributes: expected object", id)
		}

		managerID, _ := sessionAttrs["manager_id"].(string)
		if managerID == "" {
			continue
		}

		attrs := map[string]any{"manager_id": managerID, "name": name, "work_dir": workDir}

		encodedAttrs, err := json.Marshal(attrs)
		if err != nil {
			return fmt.Errorf("marshal session %d output attributes: %w", id, err)
		}

		key := fmt.Sprintf("session:%d:opened", id)

		fingerprint := openedFingerprint(id, name, workDir)
		// A partial unique index cannot be an upsert conflict target in SQLite,
		// so idempotence comes from an explicit existence check instead.
		var existing int
		if err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM session_outbox WHERE session_id = ? AND source_key = ?`,
			id, key).Scan(&existing); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check session %d lifecycle backfill: %w", id, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_outbox
				(session_id, type, content, attributes, source_key, fingerprint, created_at)
			VALUES (?, 'session_opened', '', ?, ?, ?, ?)`,
			id, string(encodedAttrs), key, fingerprint, time.Now().UTC()); err != nil {
			return fmt.Errorf("backfill session %d lifecycle: %w", id, err)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate manager-owned sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit manager outbox backfill: %w", err)
	}

	return nil
}

// openedFingerprint mirrors sessionstore.OutputFingerprint for a session_opened
// row: typed attributes without manager_id, so a later re-enqueue of the same
// source key replays as a no-op instead of a fingerprint conflict. The two
// computations must stay byte-identical.
func openedFingerprint(sessionID int64, name, workDir string) string {
	payload := struct {
		Type       string `json:"type"`
		Content    string `json:"content"`
		SessionID  int64  `json:"session_id"`
		Attributes struct {
			Name    string `json:"name"`
			WorkDir string `json:"work_dir"`
		} `json:"attributes"`
	}{Type: "session_opened", SessionID: sessionID, Attributes: struct {
		Name    string `json:"name"`
		WorkDir string `json:"work_dir"`
	}{Name: name, WorkDir: workDir}}

	encoded, _ := json.Marshal(payload) //nolint:errchkjson // closed internal lifecycle payload is JSON-safe.
	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:])
}
