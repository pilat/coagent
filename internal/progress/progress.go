package progress

import "time"

type Context struct {
	Used        int
	Max         int
	Approximate bool
	Available   bool
}

type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	Available        bool
}

type TodoItem struct {
	ID       string
	Content  string
	Status   string
	Priority string
}

type WaitingItem struct {
	Kind        string
	Description string
	WakeAt      *time.Time
}

type Budget struct {
	State             string
	Generation        int64
	CostLimitUSD      *float64
	CostUsedUSD       *float64
	CostRemainingUSD  *float64
	DurationLimit     *time.Duration
	Elapsed           *time.Duration
	DurationRemaining *time.Duration
	FiredReason       string
}

type Snapshot struct {
	RootID               int64
	Revision             string
	DurableWatermark     int64
	OutboxWatermark      int64
	RuntimeState         string
	PersistedReason      string
	ObservedAt           time.Time
	Model                string
	RootIteration        int
	MainModelWorking     bool
	ChildCount           int
	ChildIterations      int
	Context              Context
	Lifetime             Usage
	EpisodeElapsed       *time.Duration
	Todos                []TodoItem
	LatestModelProgress  string
	Waiting              []WaitingItem
	ActiveSubagents      int
	BackgroundSubagents  int
	Budget               *Budget
	LastSemanticOutputAt *time.Time
}

// TodoCounts applies the operator-visible arithmetic: active counts only
// in_progress, remaining counts everything not finished (pending, in_progress,
// and any unknown persisted status), done counts completed, and cancelled is
// reported separately so a cancelled item never reads as success.
//
//nolint:nonamedreturns // Four same-typed counters are ambiguous without names.
func (s Snapshot) TodoCounts() (active, remaining, done, cancelled int) {
	for _, item := range s.Todos {
		switch item.Status {
		case "completed":
			done++
		case "cancelled":
			cancelled++
		case "in_progress":
			active++
			remaining++
		default:
			remaining++
		}
	}

	return active, remaining, done, cancelled
}
