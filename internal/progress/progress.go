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

//nolint:nonamedreturns // Three same-typed counters are ambiguous without names.
func (s Snapshot) TodoCounts() (current, completed, remaining int) {
	for _, item := range s.Todos {
		switch item.Status {
		case "completed", "cancelled":
			completed++
		default:
			remaining++

			if item.Status == "in_progress" {
				current++
			}
		}
	}

	return current, completed, remaining
}
