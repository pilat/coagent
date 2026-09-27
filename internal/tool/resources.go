package tool

// ResourceLifecycle retires cached session resources without exposing their implementations.
//
//nolint:iface // Shared protocol keeps runtime ownership independent of tool-stack implementations.
type ResourceLifecycle interface {
	Retire(sessionID int64) error
	Invalidate(projectID int64) error
	Close() error
}
