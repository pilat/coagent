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
	RootSessionID   int64
	InputID         int64
	ToolID          string
	Command         string
	ToolCallID      string
	CostLimitUSD    *float64
	DurationSeconds *int64
	Receipt         string
}

// CrossingReason compares costs at the same precision as checkpoint receipts.
func CrossingReason(record *Record, delta float64, observedAt time.Time) string {
	if record.DurationSeconds != nil && !observedAt.Before(record.ArmedAt.Add(time.Duration(*record.DurationSeconds)*time.Second)) {
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
