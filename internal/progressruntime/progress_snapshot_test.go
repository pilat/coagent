package progressruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

// The full /status list is rendered from progressSnapshot, so the durable TODO
// projection must already be in the canonical order todoread reports.
func TestProgressSnapshot_ProjectsTodosInCanonicalOrder(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	buildTodo := func(id string, priority string, createdAt time.Time) string {
		item := map[string]any{
			"id": id, "content": id, "status": "pending", "priority": priority,
			"created_at": createdAt.Format(time.RFC3339Nano),
		}
		if createdAt.IsZero() {
			delete(item, "created_at")
		}
		encoded, err := json.Marshal(item)
		require.NoError(t, err)

		return string(encoded)
	}

	todos := `[` + buildTodo("low", "low", base) + `,` + buildTodo("high-late", "high", base.Add(time.Minute)) + `,` +
		buildTodo("tie-b", "high", base) + `,` + buildTodo("high-early", "high", base) + `,` +
		buildTodo("tie-a", "high", base) + `,` + buildTodo("legacy", "medium", time.Time{}) + `]`

	store := staticProgressStore{facts: &sessionstore.ProgressFacts{
		RootID: 7, TodoItems: json.RawMessage(todos),
	}}
	runtime, ok := New(
		store, nil, func(int64) bool { return false }, func(int64) bool { return false }, nil, nil, nil,
	).(*runtime)
	require.True(t, ok, "New must return the concrete runtime")

	facts, err := store.CaptureProgress(context.Background(), 7)
	require.NoError(t, err)

	snapshot, err := runtime.progressSnapshot(facts, base.Add(time.Hour))
	require.NoError(t, err)

	ids := make([]string, 0, len(snapshot.Todos))
	for _, item := range snapshot.Todos {
		ids = append(ids, item.ID)
	}

	assert.Equal(t, []string{"high-early", "tie-a", "tie-b", "high-late", "legacy", "low"}, ids)
}
