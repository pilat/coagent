package git

import "errors"

// Sentinel errors for git operations.
var (
	// ErrDestinationExists is returned when the clone destination already exists.
	ErrDestinationExists = errors.New("destination already exists")
	// ErrNotARepo is returned when the path is not a git repository.
	ErrNotARepo = errors.New("not a git repository")
)
