package sessionlifecycle

import (
	"context"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

type Recovery interface {
	Start(ctx context.Context, run func(context.Context)) bool
	Close() <-chan struct{}
	Active() bool
}

var _ Recovery = (*recovery)(nil)

type recovery struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
}

func NewRecovery() Recovery {
	return &recovery{}
}

func (r *recovery) Start(ctx context.Context, run func(context.Context)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.done != nil {
		return false
	}

	recoveryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done

	go runRecovery(recoveryCtx, done, run)

	return true
}

func (r *recovery) Close() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}

	return r.done
}

func (r *recovery) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.done != nil
}

func runRecovery(ctx context.Context, done chan<- struct{}, run func(context.Context)) {
	defer close(done)
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Ctx(ctx).Named("sessionlifecycle.recovery").Error(
				"recovery_panic", zap.Any("recovered", recovered), zap.Stack("stack"),
			)
		}
	}()

	run(ctx)
}
