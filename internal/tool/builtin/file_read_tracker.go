package builtin

import "context"

// ReadRecord is the on-disk fingerprint captured when a session observes a file.
type ReadRecord struct {
	MtimeUnixNano int64
	Size          int64
	Hash          string
}

// FileReadTracker records and retrieves the current session's file fingerprints.
type FileReadTracker interface {
	LookupRead(ctx context.Context, path string) (ReadRecord, bool, error)
	RecordRead(ctx context.Context, path string, record ReadRecord) error
}
