package budget

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRecordRemaining(t *testing.T) {
	t.Parallel()
	now := time.Now()
	record := Record{
		ArmedAt: now.Add(-time.Minute), BaselineCostUSD: 10,
		CostLimitUSD: new(2.0), DurationSeconds: new(int64(120)),
	}
	cost, duration := record.Remaining(10.8, now)
	assert.InDelta(t, 1.2, *cost, 1e-9)
	assert.Equal(t, time.Minute, *duration)
	cost, duration = record.Remaining(15, now.Add(5*time.Minute))
	assert.Zero(t, *cost)
	assert.Zero(t, *duration)
	_, duration = record.Remaining(10, now.Add(-2*time.Minute))
	assert.Nil(t, duration)
	cost, duration = (Record{}).Remaining(10, now)
	assert.Nil(t, cost)
	assert.Nil(t, duration)
}
