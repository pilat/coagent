package ctl

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// Serve accepts connections and answers ops from the first one. A daemon that
// assembles its control plane after binding uses ServeStarting instead.
func (s *Server) Serve(ctx context.Context) error {
	s.MarkReady()

	return s.ServeStarting(ctx)
}

// ServeStarting accepts before the daemon is ready — connect success is the liveness
// test. Every op replies CodeStarting until MarkReady, so registering now is safe.
func (s *Server) ServeStarting(ctx context.Context) error {
	s.mu.Lock()
	if s.serving {
		s.mu.Unlock()

		return ErrAlreadyServing
	}

	s.serving = true
	close(s.serveReady)
	s.mu.Unlock()

	return s.accept(ctx)
}

// MarkReady opens the registered ops and closes registration. Idempotent: the
// restart path and Serve both reach it.
func (s *Server) MarkReady() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ready = true
}

func (s *Server) accept(ctx context.Context) error {
	log := logger.Named("ctl")

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}

			return fmt.Errorf("accept: %w", err)
		}

		s.wg.Go(func() {
			if err := s.handleConn(ctx, conn); err != nil {
				log.Debug("connection_ended", zap.Error(err))
			}
		})
	}
}

func (s *Server) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ready
}
