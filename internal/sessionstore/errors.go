package sessionstore

import "errors"

var (
	ErrAssistantCommand = errors.New("commands belong to the user; use set_model, set_budget or stop")
	ErrChatNotBound     = errors.New("chat is not bound yet")
)
