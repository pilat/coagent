package budget

import (
	"errors"
	"math"
	"time"
)

const (
	Armed    State = "armed"
	Fired    State = "fired"
	Released State = "released"
)

var (
	ErrNotFound = errors.New("session budget not found")
	ErrConflict = errors.New("session budget conflict")
)

type State string

type Record struct {
	RootSessionID   int64
	State           State
	Generation      int64
	ArmedAt         time.Time
	BaselineCostUSD float64
	CostLimitUSD    *float64
	DurationSeconds *int64
	FiredAt         *time.Time
	ReleasedAt      *time.Time
	FiredReason     string
	ReleasedReason  string
	ObservedCostUSD *float64
	ParkPhase       string
	ParkOwner       string
}

type Mutation struct {
	RootSessionID int64
	// GrantSessionID receives the receipt; zero uses the root session.
	GrantSessionID  int64
	ToolCallID      string
	CostLimitUSD    *float64
	DurationSeconds *int64
	Receipt         string
}

// Remaining uses the root tree's cumulative cost and the budget's original baseline.
func (r Record) Remaining(treeCostUSD float64, now time.Time) (*float64, *time.Duration) {
	var cost *float64
	var duration *time.Duration

	if r.CostLimitUSD != nil {
		remaining := max(*r.CostLimitUSD-(treeCostUSD-r.BaselineCostUSD), 0)
		cost = &remaining
	}

	if r.DurationSeconds != nil && !now.Before(r.ArmedAt) {
		remaining := max(time.Duration(*r.DurationSeconds)*time.Second-now.Sub(r.ArmedAt), 0)
		duration = &remaining
	}

	return cost, duration
}

// CrossingReason compares costs at the same precision as checkpoint receipts.
func CrossingReason(record *Record, delta float64, observedAt time.Time) string {
	if record.DurationSeconds != nil &&
		!observedAt.Before(record.ArmedAt.Add(time.Duration(*record.DurationSeconds)*time.Second)) {
		return "duration"
	}

	if record.CostLimitUSD != nil && RoundCostUSD(delta) >= RoundCostUSD(*record.CostLimitUSD) {
		return "cost"
	}

	return ""
}

func RoundCostUSD(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}
