package daemon

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/session"
)

type runnerInfo struct {
	WorkDir         string
	ProjectID       int64
	Child           bool
	ParentID        int64
	PreserveStopped bool
}

type runner struct {
	mu sync.Mutex

	cancel          context.CancelFunc
	done            chan struct{}
	service         *session.Session
	working         bool
	hasRun          bool
	workDir         string
	projectID       int64
	child           bool
	parentID        int64
	preserveStopped bool
}

func (r *runner) Cancel() { r.cancel() }

func (r *runner) Stop() {
	r.cancel()
	<-r.done
}

func (r *runner) Done() <-chan struct{} { return r.done }

func (r *runner) Complete() { close(r.done) }

func (r *runner) Service() *session.Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.service
}

func (r *runner) SetService(service *session.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.service = service
}

func (r *runner) Working() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.working
}

func (r *runner) SetWorking(working bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.working = working
}

func (r *runner) SetPreserveStopped(preserve bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.preserveStopped = preserve
}

func (r *runner) HasRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hasRun
}

func (r *runner) MarkRun() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.hasRun = true
}

func (r *runner) Info() runnerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	return runnerInfo{
		WorkDir: r.workDir, ProjectID: r.projectID, Child: r.child,
		ParentID: r.parentID, PreserveStopped: r.preserveStopped,
	}
}

func newRunner(
	cancel context.CancelFunc,
	workDir string,
	projectID int64,
	child bool,
	parentID int64,
	preserveStopped bool,
) *runner {
	return &runner{
		cancel: cancel, done: make(chan struct{}), workDir: workDir,
		projectID: projectID, child: child, parentID: parentID,
		preserveStopped: preserveStopped,
	}
}
