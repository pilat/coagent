package lsp

import "errors"

var (
	ErrClientExited      = errors.New("lsp client exited")
	ErrServerUnavailable = errors.New("lsp server unavailable")
)
