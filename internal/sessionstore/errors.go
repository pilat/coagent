package sessionstore

import "errors"

var (
	ErrChatNotBound = errors.New("chat is not bound yet")
	ErrBudgetParked = errors.New(
		"is parked at its budget checkpoint; raise or clear its budget with set_budget first",
	)
	ErrForwardedCommand  = errors.New("slash commands stay on #0; forward refuses them")
	ErrForwardInput      = errors.New("forward needs a user message in this turn; this turn has none")
	ErrOutputNotWritable = errors.New("cannot commit ordinary output")
)
