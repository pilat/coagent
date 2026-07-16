package session

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// heartbeatTicker sends periodic heartbeat signals while the agent is working.
type heartbeatTicker struct {
	fn     func(context.Context)
	mu     sync.Mutex
	cancel context.CancelFunc
}

func newHeartbeatTicker(fn func(context.Context)) *heartbeatTicker {
	return &heartbeatTicker{fn: fn}
}

func (h *heartbeatTicker) start(ctx context.Context) {
	if h.fn == nil {
		return
	}

	log := logger.Ctx(ctx).Named("session.heartbeat")

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cancel != nil {
		return // already running
	}

	childCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Warn("heartbeat_panic_recovered", zap.Any("error", r))
						}
					}()

					h.fn(childCtx)
				}()
			case <-childCtx.Done():
				return
			}
		}
	}()
}

func (h *heartbeatTicker) stop() {
	if h.fn == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}
