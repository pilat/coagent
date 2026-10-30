package sessionstore

import "errors"

var (
	ErrChatNotBound = errors.New("chat is not bound yet")
	ErrBudgetParked = errors.New("is parked at its budget checkpoint; raise or clear its budget with set_budget first")
)
