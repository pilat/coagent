package sessionstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitingOutputChainRetainsRepeatedSet(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	var previous int64
	ids := make([]int64, 0, 3)
	for _, set := range []string{"a", "a+b", "a"} {
		waiting, identity := waitingTestPayload(set)
		commit, err := store.EnqueueOutput(ctx, OutputDraft{
			SessionID: record.ID,
			Type:      OutputMessageReplaceable,
			Content:   set,
			Attributes: map[string]any{
				"waiting": waiting, "waiting_identity": identity,
			},
			SourceKey: fmt.Sprintf("wait:%d:%s", previous, set),
			Fingerprint: OutputFingerprint(
				OutputMessageReplaceable,
				set,
				record.ID,
				map[string]any{
					"waiting": waiting, "waiting_identity": identity,
				},
			),
		})
		require.NoError(t, err)
		previous = commit.OutputID
		ids = append(ids, commit.OutputID)
	}

	assert.Len(t, map[int64]struct{}{ids[0]: {}, ids[1]: {}, ids[2]: {}}, 3)
	waiting, ok := store.(WaitingOutputStore)
	require.True(t, ok)
	latest, err := waiting.LatestWaitingOutput(ctx, record.ID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, ids[2], latest.ID)
	assert.Equal(t, fmt.Sprintf("wait:%d:a", ids[1]), latest.SourceKey)
}

func waitingTestPayload(set string) ([]map[string]any, []map[string]any) {
	waiting := []map[string]any{{"wake_at": "2026-08-26T00:00:00Z"}}
	identity := []map[string]any{{"tool_call_id": "a"}}
	if set == "a+b" {
		waiting = append(waiting, map[string]any{"child_id": 7})
		identity = append(identity, map[string]any{"child_id": 7, "activation_seq": 1})
	}

	return waiting, identity
}
