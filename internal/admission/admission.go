package admission

import (
	"errors"
	"sync"
	"sync/atomic"
)

const (
	MaxTotal     = 16
	MaxChildren  = 12
	MaxPerParent = 8
	MaxDepth     = 3
)

// ErrNoCapacity is the retryable runner-admission verdict.
var ErrNoCapacity = errors.New("session capacity reached")

// Kind separates root reservations from subagent reservations.
type Kind int

const (
	Parent Kind = iota
	Child
)

// Governor owns concurrent runner capacity and per-parent child quotas.
type Governor interface {
	TryAdmit(kind Kind, parentID int64) bool
	Release(kind Kind, parentID int64)
	CanAdmitChild(parentID int64) bool
	LiveTotal() int64
	LiveChildren() int64
}

var _ Governor = (*governor)(nil)

type governor struct {
	mu              sync.Mutex
	running         int
	runningChildren int
	perParent       map[int64]int

	totalGauge atomic.Int64
	childGauge atomic.Int64
}

func New() Governor {
	return &governor{perParent: make(map[int64]int)}
}

func (g *governor) TryAdmit(kind Kind, parentID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running >= MaxTotal {
		return false
	}

	if kind == Child && (g.runningChildren >= MaxChildren || g.perParent[parentID] >= MaxPerParent) {
		return false
	}

	g.running++
	g.totalGauge.Store(int64(g.running))

	if kind == Child {
		g.runningChildren++
		g.perParent[parentID]++
		g.childGauge.Store(int64(g.runningChildren))
	}

	return true
}

func (g *governor) Release(kind Kind, parentID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.running > 0 {
		g.running--
	}

	g.totalGauge.Store(int64(g.running))

	if kind != Child {
		return
	}

	if g.runningChildren > 0 {
		g.runningChildren--
	}

	g.childGauge.Store(int64(g.runningChildren))

	if g.perParent[parentID] > 0 {
		g.perParent[parentID]--
		if g.perParent[parentID] == 0 {
			delete(g.perParent, parentID)
		}
	}
}

func (g *governor) CanAdmitChild(parentID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.running < MaxTotal &&
		g.runningChildren < MaxChildren &&
		g.perParent[parentID] < MaxPerParent
}

func (g *governor) LiveTotal() int64 {
	return g.totalGauge.Load()
}

func (g *governor) LiveChildren() int64 {
	return g.childGauge.Load()
}
