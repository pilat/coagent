package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/transcript"
)

type sessionEvents struct {
	daemon    *svc
	sessionID int64
}

func (e *sessionEvents) Emit(n sessionevent.Notification) {
	s := e.daemon
	ctx := context.Background()

	switch n.Type {
	case sessionevent.NotifyModelWorking:
		working, _ := n.Attributes["working"].(bool)
		if rs, exists := s.runners.Load(e.sessionID); exists {
			rs.SetWorking(working)

			if working {
				rs.SetPreserveStopped(false)
			}
		}

		s.updateLive(ctx, e.sessionID)
		s.progress.Wake()
	case sessionevent.NotifyContextChanged:
		s.updateLive(ctx, e.sessionID)
	case sessionevent.NotifyIterationPersisted:
		if iteration, ok := n.Attributes["iteration"].(int); ok {
			s.publishSubagentIterationProgress(ctx, e.sessionID, int64(iteration))
		}
	case sessionevent.NotifyProgressChanged:
		s.updateLive(ctx, e.sessionID)

		content, published, err := s.progress.EnqueueChange(ctx, e.sessionID)
		if err != nil {
			logger.Ctx(ctx).Named("daemon.progress").Warn("enqueue_progress_change", zap.Error(err))
			return
		}

		if published {
			s.publish(e.sessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: content})
		}
	case sessionevent.NotifyMessage,
		sessionevent.NotifyHeartbeat,
		sessionevent.NotifyStateChanged,
		sessionevent.NotifyInputReceived,
		sessionevent.NotifySessionCreated,
		sessionevent.NotifySessionCleared,
		sessionevent.NotifyWaiting:
		s.publish(e.sessionID, n)
	default:
		s.publish(e.sessionID, n)
	}
}

func (s *svc) startInboxWake(ctx context.Context) {
	workerCtx, cancel := s.newDaemonWorkerContext(ctx)

	s.workerWG.Go(func() {
		defer cancel()
		defer func() {
			if value := recover(); value != nil {
				logger.Ctx(workerCtx).
					Named("daemon.input").
					Error("wake_panic", zap.Any("panic", value), zap.Stack("stack"))
			}
		}()

		for {
			select {
			case <-workerCtx.Done():
				return
			case <-s.store.Woken():
				for _, id := range s.store.TakeWoken() {
					s.refreshBudgetTimer(workerCtx, id)

					if err := s.inputReady(workerCtx, id); err != nil {
						logger.Ctx(workerCtx).
							Named("daemon.input").
							Warn("input_ready_failed", zap.Int64("session_id", id), zap.Error(err))
					}
				}
			}
		}
	})
}

func storedWireMessages(messages []*transcript.Message) ([]llmwire.Message, error) {
	rows := make([]llmwire.Message, 0, len(messages))
	for _, message := range messages {
		row := llmwire.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		if len(message.ToolCalls) > 0 {
			if err := json.Unmarshal(message.ToolCalls, &row.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode tool calls of message %d: %w", message.ID, err)
			}
		}

		rows = append(rows, row)
	}

	return rows, nil
}
