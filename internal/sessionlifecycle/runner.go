package sessionlifecycle

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/session"
)

type Runner[T any] interface {
	Cancel()
	Stop()
	Done() <-chan struct{}
	Complete()
	AppendInput(T)
	DrainInputs() []T
	Service() session.Service
	SetService(session.Service)
	HasRun() bool
	MarkRun()
	Info() RunnerInfo
}

type RunnerInfo struct {
	WorkDir         string
	ProjectID       int64
	Kind            admission.Kind
	ParentID        int64
	PreserveStopped bool
}

var _ Runner[int] = (*runner[int])(nil)

type runner[T any] struct {
	mu sync.Mutex

	cancel          context.CancelFunc
	done            chan struct{}
	service         session.Service
	inputs          []T
	hasRun          bool
	workDir         string
	projectID       int64
	kind            admission.Kind
	parentID        int64
	preserveStopped bool
}

func NewRunner[T any](
	cancel context.CancelFunc,
	workDir string,
	projectID int64,
	kind admission.Kind,
	parentID int64,
	preserveStopped bool,
	inputs []T,
) Runner[T] {
	return &runner[T]{
		cancel: cancel, done: make(chan struct{}), workDir: workDir,
		projectID: projectID, kind: kind, parentID: parentID,
		preserveStopped: preserveStopped, inputs: inputs,
	}
}

func (r *runner[T]) Cancel() { r.cancel() }

func (r *runner[T]) Stop() {
	r.cancel()
	<-r.done
}

func (r *runner[T]) Done() <-chan struct{} { return r.done }

func (r *runner[T]) Complete() { close(r.done) }

func (r *runner[T]) AppendInput(input T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.inputs = append(r.inputs, input)
}

func (r *runner[T]) DrainInputs() []T {
	r.mu.Lock()
	defer r.mu.Unlock()

	inputs := r.inputs
	r.inputs = nil

	return inputs
}

func (r *runner[T]) Service() session.Service {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.service
}

func (r *runner[T]) SetService(service session.Service) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.service = service
}

func (r *runner[T]) HasRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.hasRun
}

func (r *runner[T]) MarkRun() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.hasRun = true
}

func (r *runner[T]) Info() RunnerInfo {
	return RunnerInfo{
		WorkDir: r.workDir, ProjectID: r.projectID, Kind: r.kind,
		ParentID: r.parentID, PreserveStopped: r.preserveStopped,
	}
}
