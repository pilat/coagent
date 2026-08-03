package managers

import "context"

type Manager interface {
	ID() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// Alive reports whether the manager's own loops are still running, so
	// `status` cannot keep claiming one that died after Start returned.
	Alive() bool
}
