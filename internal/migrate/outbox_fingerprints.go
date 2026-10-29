package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pressly/goose/v3"
)

type outboxFingerprintRow54 struct {
	id         int64
	sessionID  int64
	kind       string
	content    string
	attributes map[string]any
	releases   bool
}

func outboxFingerprintsMigration() *goose.Migration {
	return goose.NewGoMigration(54, &goose.GoFunc{RunDB: migrateOutboxFingerprints54}, nil)
}

func migrateOutboxFingerprints54(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin output fingerprint migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := fingerprintRows54(ctx, tx)
	if err != nil {
		return err
	}

	for _, row := range rows {
		fingerprint := outputFingerprint54(row)
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE session_outbox SET fingerprint = ? WHERE id = ?`,
			fingerprint,
			row.id,
		); err != nil {
			return fmt.Errorf("rewrite output %d fingerprint: %w", row.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit output fingerprint migration: %w", err)
	}

	return nil
}

func fingerprintRows54(ctx context.Context, tx *sql.Tx) ([]outboxFingerprintRow54, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, type, content, attributes, releases_input
		FROM session_outbox WHERE source_key IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read output fingerprints: %w", err)
	}
	defer rows.Close()

	var result []outboxFingerprintRow54

	for rows.Next() {
		var row outboxFingerprintRow54

		var attributes string
		if err := rows.Scan(&row.id, &row.sessionID, &row.kind, &row.content, &attributes, &row.releases); err != nil {
			return nil, fmt.Errorf("scan output fingerprint: %w", err)
		}

		decoder := json.NewDecoder(strings.NewReader(attributes))
		decoder.UseNumber()

		if err := decoder.Decode(&row.attributes); err != nil {
			return nil, fmt.Errorf("decode output %d attributes: %w", row.id, err)
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate output fingerprints: %w", err)
	}

	return result, nil
}

// Freeze this formula: later runtime changes must not change historical upgrades.
func outputFingerprint54(row outboxFingerprintRow54) string {
	payload := struct {
		Type          string          `json:"type"`
		Content       string          `json:"content"`
		SessionID     int64           `json:"session_id"`
		Attributes    json.RawMessage `json:"attributes"`
		ReleasesInput bool            `json:"releases_input,omitempty"`
	}{row.kind, row.content, row.sessionID, fingerprintAttributes54(row.kind, row.attributes), row.releases}
	encoded, _ := json.Marshal(payload) //nolint:errchkjson // The frozen payload contains only typed JSON values.
	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:])
}

func fingerprintAttributes54(kind string, attributes map[string]any) json.RawMessage {
	type messageAttributes struct {
		Source string `json:"source,omitempty"`
	}
	type openedAttributes struct {
		Name    string `json:"name"`
		WorkDir string `json:"work_dir"`
	}
	type replacedAttributes struct {
		OldSessionID int64  `json:"old_session_id"`
		NewSessionID int64  `json:"new_session_id"`
		Name         string `json:"name"`
		WorkDir      string `json:"work_dir"`
	}
	type closedAttributes struct {
		Reason string `json:"reason"`
	}
	text := func(key string) string { value, _ := attributes[key].(string); return value }
	id := func(key string) int64 {
		value, _ := attributes[key].(json.Number)
		number, _ := value.Int64()

		return number
	}
	var value any = struct{}{}

	switch kind {
	case "message":
		value = messageAttributes{Source: text("source")}
	case "session_opened":
		value = openedAttributes{Name: text("name"), WorkDir: text("work_dir")}
	case "session_replaced":
		value = replacedAttributes{
			OldSessionID: id("old_session_id"),
			NewSessionID: id("new_session_id"),
			Name:         text("name"),
			WorkDir:      text("work_dir"),
		}
	case "session_closed":
		value = closedAttributes{Reason: text("reason")}
	}

	encoded, _ := json.Marshal(value) //nolint:errchkjson // The frozen variants contain JSON primitives only.

	return encoded
}
