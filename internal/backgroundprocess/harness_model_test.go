package backgroundprocess

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deliveryModel struct {
	state  string
	target int64
}

func TestHarnessModel_FirstTerminalCauseWinsAcrossOrderings(t *testing.T) {
	intents := []HostIntent{
		IntentDeadline,
		IntentOutputLimit,
		IntentSessionStopped,
		IntentSessionKilled,
		IntentDaemonShutdown,
	}

	for _, intent := range intents {
		for _, intentFirst := range []bool{false, true} {
			name := fmt.Sprintf("%s/intent_first=%t", intent, intentFirst)
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store := newTestStore(t)
				record := runningRecord(t, store, 2)

				if intentFirst {
					won, err := store.RecordIntent(ctx, record.ID, intent)
					require.NoError(t, err)
					require.True(t, won)
				}

				zero := 0
				final, won, err := store.Finalize(ctx, record.ID, StateCompleted, &zero, 1)
				require.NoError(t, err)
				require.True(t, won)

				if intentFirst {
					assert.Equal(t, IntentToState(intent), final.State)
				} else {
					assert.Equal(t, StateCompleted, final.State)
					won, err := store.RecordIntent(ctx, record.ID, intent)
					require.NoError(t, err)
					assert.False(t, won)
				}

				if intentFirst && (intent == IntentSessionStopped || intent == IntentSessionKilled) {
					assert.Equal(t, "suppressed", final.DeliveryState)
				} else {
					assert.Equal(t, "pending", final.DeliveryState)
				}
			})
		}
	}
}

func (m *deliveryModel) claim(ownerActive bool) bool {
	if m.state != "pending" {
		return false
	}

	m.state = "claimed"
	m.target = 1
	if ownerActive {
		m.target = 2
	}

	return true
}

func (m *deliveryModel) deliver() bool {
	if m.state != "claimed" {
		return false
	}

	m.state = "delivered"

	return true
}

func TestHarnessModel_ProcessDeliveryClaimAndRestart(t *testing.T) {
	tests := []struct {
		name        string
		ownerStatus string
		ownerActive bool
	}{
		{name: "live owner", ownerStatus: "suspended", ownerActive: true},
		{name: "terminal owner", ownerStatus: "completed", ownerActive: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			setTestSessionStatus(t, store, 2, tt.ownerStatus)
			record := runningRecord(t, store, 2)
			zero := 0
			_, won, err := store.Finalize(ctx, record.ID, StateCompleted, &zero, 0)
			require.NoError(t, err)
			require.True(t, won)

			model := &deliveryModel{state: "pending"}
			wantWon := model.claim(tt.ownerActive)
			target, won, err := store.ClaimDelivery(ctx, record.ID)
			require.NoError(t, err)
			assert.Equal(t, wantWon, won)
			assert.Equal(t, model.target, target)

			wantWon = model.claim(tt.ownerActive)
			_, won, err = store.ClaimDelivery(ctx, record.ID)
			require.NoError(t, err)
			assert.Equal(t, wantWon, won)

			undelivered, err := store.ListUndelivered(ctx)
			require.NoError(t, err)
			require.Len(t, undelivered, 1)
			assert.Equal(t, model.state, undelivered[0].DeliveryState)
			assert.Equal(t, model.target, undelivered[0].DeliveryTargetSessionID)

			wantWon = model.deliver()
			won, err = store.MarkDelivered(ctx, record.ID)
			require.NoError(t, err)
			assert.Equal(t, wantWon, won)

			undelivered, err = store.ListUndelivered(ctx)
			require.NoError(t, err)
			assert.Empty(t, undelivered)

			final, err := store.GetProcess(ctx, record.ID)
			require.NoError(t, err)
			assert.Equal(t, model.state, final.DeliveryState)
			assert.Equal(t, model.target, final.DeliveryTargetSessionID)
		})
	}
}
