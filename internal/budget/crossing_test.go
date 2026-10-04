package budget

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Comparison precision matches checkpoint receipts, so float dust cannot change the verdict.
func TestBudgetCrossingReason(t *testing.T) {
	t.Parallel()

	armedAt := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	limit := 5.0

	costBudget := &Record{
		State: Armed, CostLimitUSD: &limit, BaselineCostUSD: 0, ArmedAt: armedAt,
	}
	durationBudget := &Record{
		State: Armed, DurationSeconds: new(int64(3600)), BaselineCostUSD: 0, ArmedAt: armedAt,
	}
	bothBudget := &Record{
		State: Armed, CostLimitUSD: &limit, DurationSeconds: new(int64(3600)),
		BaselineCostUSD: 0, ArmedAt: armedAt,
	}

	tests := []struct {
		name       string
		record     *Record
		delta      float64
		observedAt time.Time
		want       string
	}{
		{
			name: "dust below the persisted precision still crosses",
			// 4.9999996 rounds to 5.000000 at the precision receipts use.
			record: costBudget, delta: 4.9999996, observedAt: armedAt.Add(time.Minute), want: "cost",
		},
		{
			name:       "half a microdollar short does not cross",
			record:     costBudget,
			delta:      4.9999994,
			observedAt: armedAt.Add(time.Minute),
			want:       "",
		},
		{
			name:       "exactly at the limit crosses",
			record:     costBudget,
			delta:      5,
			observedAt: armedAt.Add(time.Minute),
			want:       "cost",
		},
		{
			name:       "duration deadline crossed at observation wins over cost",
			record:     bothBudget,
			delta:      5,
			observedAt: armedAt.Add(time.Hour),
			want:       "duration",
		},
		{
			name:       "cost wins while the duration deadline is ahead",
			record:     bothBudget,
			delta:      5,
			observedAt: armedAt.Add(30 * time.Minute),
			want:       "cost",
		},
		{
			name:       "duration-only budget crosses at its deadline",
			record:     durationBudget,
			delta:      0,
			observedAt: armedAt.Add(time.Hour),
			want:       "duration",
		},
		{
			name:       "duration-only budget does not cross before its deadline",
			record:     durationBudget,
			delta:      0,
			observedAt: armedAt.Add(30 * time.Minute),
			want:       "",
		},
		{
			name:       "below both limits does not cross",
			record:     costBudget,
			delta:      1,
			observedAt: armedAt.Add(time.Minute),
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, CrossingReason(tt.record, tt.delta, tt.observedAt))
		})
	}
}
