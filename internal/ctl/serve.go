package ctl

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// ServeStarting accepts before the daemon is ready — connect success is the liveness
// test. Every op replies CodeStarting until MarkReady, so booting never reads as down.
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

// MarkReady opens the status op. Idempotent: the restart path reaches it.
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
