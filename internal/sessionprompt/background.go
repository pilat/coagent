package sessionprompt

// BackgroundSectionMarker identifies the host-authored snapshot inside checkpoints.
const BackgroundSectionMarker = "\n\n# Active background work\n"

// ActiveSubagentInfo captures an in-flight child at activation start.
type ActiveSubagentInfo struct {
	ChildID  int64
	Blocking bool
	State    string
}

// ActiveProcessInfo captures an advertised process at activation start.
type ActiveProcessInfo struct {
	ID         string
	OutputPath string
}
