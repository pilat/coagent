package budget

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCost(t *testing.T) {
	t.Parallel()

	negative := -1.0
	zero := 0.0
	over := 1_000_000.01
	precise := 1.2345649
	normalized := 1.234565
	costErr := "cost_usd must be finite, positive, and no more than 1000000"

	exactCap := 1_000_000.0

	tests := []struct {
		name    string
		value   *float64
		want    *float64
		wantErr string
	}{
		{name: "omitted stays omitted", value: nil, want: nil},
		{name: "valid cost passes", value: &precise, want: &normalized},
		{name: "exact cap is allowed", value: &exactCap, want: &exactCap},
		{name: "zero rejected", value: &zero, wantErr: costErr},
		{name: "negative rejected", value: &negative, wantErr: costErr},
		{name: "over cap rejected", value: &over, wantErr: costErr},
		{name: "NaN rejected", value: ptrFloat(math.NaN()), wantErr: costErr},
		{name: "Inf rejected", value: ptrFloat(math.Inf(1)), wantErr: costErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeCost(tt.value)

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			if tt.want == nil {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.InDelta(t, *tt.want, *got, 1e-12)
		})
	}
}

func ptrFloat(value float64) *float64 { return &value }

// TestParseRelativeDuration pins the relative-only subset: Go durations and the
// integer d/w suffixes pass; RFC3339 timestamps must never sneak through.
func TestParseRelativeDuration(t *testing.T) {
	t.Parallel()

	week := 7 * 24 * time.Hour
	minuteAndAHalf := 90 * time.Second

	tests := []struct {
		name    string
		value   string
		want    *time.Duration
		wantErr string
	}{
		{name: "empty is omitted", value: "", want: nil},
		{name: "go duration", value: "90s", want: &minuteAndAHalf},
		{name: "exact one minute boundary", value: "1m", want: ptrDuration(time.Minute)},
		{name: "exact 365 day boundary", value: "365d", want: ptrDuration(365 * 24 * time.Hour)},
		{name: "day suffix", value: "3d", want: ptrDuration(72 * time.Hour)},
		{name: "week suffix", value: "1w", want: &week},
		{name: "below one minute", value: "30s", wantErr: "duration must be between 1 minute and 365 days"},
		{name: "over one year", value: "400d", wantErr: "duration must be between 1 minute and 365 days"},
		{
			name:    "rfc3339 timestamp rejected",
			value:   "2026-08-27T10:00:00Z",
			wantErr: "duration must be relative, not an RFC3339 timestamp",
		},
		{name: "garbage rejected", value: "soon", wantErr: `invalid duration "soon"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseRelativeDuration(tt.value)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			if tt.want == nil {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, *tt.want, *got)
		})
	}
}

func ptrDuration(value time.Duration) *time.Duration { return &value }
