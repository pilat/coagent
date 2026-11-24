package sessionstoretest

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// BareRoot inserts a root row without creating output or accepting input.
func BareRoot(t testing.TB, db *sql.DB, projectID int64, model, reasoningLevel string, attrs map[string]any) int64 {
	t.Helper()

	if reasoningLevel == "" {
		reasoningLevel = "medium"
	}

	if attrs == nil {
		attrs = map[string]any{}
	}

	raw, err := json.Marshal(attrs)
	if err != nil {
		t.Fatalf("marshal root attributes: %v", err)
	}

	now := time.Now().UTC()

	result, err := db.ExecContext(t.Context(), `INSERT INTO sessions
		(project_id, model, reasoning_level, attributes, agent_type, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, projectID, model, reasoningLevel, string(raw), "build", now, now)
	if err != nil {
		t.Fatalf("insert root: %v", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("root id: %v", err)
	}

	return id
}
