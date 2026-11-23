package controllerapi

import "context"

// LineageController follows an owned root through its /clear replacements.
//
//nolint:iface // optional capability asserted by the local API server.
type LineageController interface {
	// Successor is the root that replaced sessionID through /clear; zero while none has.
	Successor(ctx context.Context, sessionID int64) (int64, error)
}
