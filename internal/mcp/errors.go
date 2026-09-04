package mcp

import "errors"

// errPoolStopped is returned once Stop has run.
var errPoolStopped = errors.New("pool is stopped")

// errInvalidated is returned when a start finishes after Invalidate covered its
// server: the freshly started subprocess is discarded, never pooled.
var errInvalidated = errors.New("mcp server was invalidated while starting")
