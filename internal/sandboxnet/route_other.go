//go:build !linux

package sandboxnet

import (
	"context"
	"errors"
	"os"
)

// ErrRouterUnsupported reports that sandbox routing needs Linux.
var ErrRouterUnsupported = errors.New("sandbox network routing requires Linux")

// Router is unavailable outside Linux.
type Router struct{}

// NewRouter is unavailable outside Linux.
func NewRouter(context.Context, *os.File, Config) (*Router, error) { return nil, ErrRouterUnsupported }

// Cutoff is unavailable outside Linux.
func (*Router) Cutoff() error { return ErrRouterUnsupported }

// Stop is unavailable outside Linux.
func (*Router) Stop(context.Context) error { return nil }
