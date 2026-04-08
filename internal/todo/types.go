package todo

import "time"

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusCancelled  Status = "cancelled"
)

const (
	PriorityHigh   Priority = "high"
	PriorityMedium Priority = "medium"
	PriorityLow    Priority = "low"
)

type (
	Status   string
	Priority string

	Item struct {
		ID        string    `json:"id"`
		Content   string    `json:"content"`
		Status    Status    `json:"status"`
		Priority  Priority  `json:"priority"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
)
