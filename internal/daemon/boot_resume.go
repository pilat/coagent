package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func (s *svc) resumeAfterRestart(ctx context.Context) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	// PASS 1 — resume children that were still running. On completion their exit
	// calls finalizeChild → deliverCompletionToParent, reviving the parent lazily.
	running, runningErr := s.links.ListRunningChildLinks(ctx)
	if runningErr != nil {
		log.Error("list_running_child_links", zap.Error(runningErr))
	}

	for _, link := range running {
		s.resumeChild(ctx, link)
	}

	// PASS 2 — re-deliver terminal-but-undelivered completions to live/idle parents.
	undelivered, undeliveredErr := s.links.ListUndeliveredParentLinks(ctx)
	if undeliveredErr != nil {
		log.Error("list_undelivered_parent_links", zap.Error(undeliveredErr))
	}

	for _, link := range undelivered {
		s.deliverCompletionToParent(ctx, link)
	}

	// PASS 3 — recover accepted normal input committed after a runner's last drain.
	resumedInput, inputErr := s.resumeSessionsWithRecoverableInput(ctx)

	counts := []zap.Field{
		zap.Int("resumed", len(running)),
		zap.Int("redelivered", len(undelivered)),
		zap.Int("input_resumed", resumedInput),
	}

	// A failed pass leaves children unresumed and completions undelivered — that
	// must never be reported with the same line as a clean recovery.
	if runningErr != nil || undeliveredErr != nil || inputErr != nil {
		log.Error("sweep_incomplete", append(counts,
			zap.Bool("running_failed", runningErr != nil),
			zap.Bool("undelivered_failed", undeliveredErr != nil),
			zap.Bool("input_failed", inputErr != nil),
		)...)

		return
	}

	log.Info("sweep_done", counts...)
}

// resumeSessionsWithRecoverableInput is sweep PASS 3: normal input either still
// queued or promoted into an unfinished user turn before the process stopped.
// Roots start immediately; children preserve the activation ordering barrier —
// running links were handled in PASS 1, terminal links rearm only once their old
// completion is delivered, and stopped links require an explicit follow-up.
func (s *svc) resumeSessionsWithRecoverableInput(ctx context.Context) (int, error) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	sessionIDs, listErr := s.store.ListSessionsWithRecoverableInput(ctx)
	if listErr != nil {
		log.Error("list_recoverable_session_input", zap.Error(listErr))

		listErr = fmt.Errorf("list sessions with recoverable input: %w", listErr)
	}

	resumed := 0

	for _, sessionID := range sessionIDs {
		link, err := s.links.GetLink(ctx, sessionID)
		if err != nil {
			log.Error("classify_recoverable_session", zap.Int64("session_id", sessionID), zap.Error(err))
			continue
		}

		switch {
		case link == nil:
			started, resumeErr := s.resumeRecoverableRoot(ctx, sessionID)
			if resumeErr != nil {
				log.Error("resume_recoverable_session", zap.Int64("session_id", sessionID), zap.Error(resumeErr))
				continue
			}

			if started {
				resumed++
			}
		case link.State == subagent.StateStopped:
			if err := s.resumeRecoverableChild(ctx, sessionID); err != nil {
				log.Error("resume_stopped_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
		case link.State == subagent.StateError && link.DeliveredAt != 0:
			if err := s.resumeRecoverableChild(ctx, sessionID); err != nil {
				log.Error("resume_errored_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
		case link.Terminal() && link.DeliveredAt != 0:
			if err := s.rearmChildAfterDelivery(ctx, sessionID); err != nil {
				log.Error("rearm_pending_child", zap.Int64("child", sessionID), zap.Error(err))
				continue
			}

			resumed++
		}
	}

	return resumed, listErr
}

func (s *svc) resumeRecoverableRoot(ctx context.Context, sessionID int64) (bool, error) {
	inputs, inputErr := s.store.ListPending(ctx, sessionID)
	if inputErr != nil {
		return false, fmt.Errorf("resume recoverable root: %w", inputErr)
	}

	for _, input := range inputs {
		if input.Source != sessionstore.InputSourceUser {
			continue
		}

		switch strings.TrimSpace(input.RawContent) {
		case statusCommand:
			if _, err := s.handleGenericCommand(ctx, input); err != nil {
				return false, err
			}
		case "/stop", "/clear", "/kill":
			return s.handleGenericCommand(ctx, input)
		}
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return false, err
	}
	defer unlock()

	ctx = context.WithoutCancel(ctx)

	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("load recoverable root: %w", err)
	}

	runnable, err := s.recoverableInputRunnable(ctx, sessionID)
	if err != nil || !runnable {
		return false, err
	}

	if record.Status == sessionstore.SessionStatusStopped {
		preserveStopped, preserveErr := s.commandOnlyStoppedRoot(ctx, record)
		if preserveErr != nil {
			return false, preserveErr
		}

		if !preserveStopped {
			if err := s.store.UpdateSessionStatus(ctx, sessionID, sessionstore.SessionStatusActive); err != nil {
				return false, fmt.Errorf("resume stopped root: %w", err)
			}
		}
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return false, fmt.Errorf("resolve recoverable root project: %w", err)
	}

	if err := s.ensureRunnerLocked(ctx, sessionID, workDir, record.ProjectID); err != nil {
		if errors.Is(err, errNoCapacity) {
			s.enqueuePendingRunner(sessionID, workDir, record.ProjectID)

			return true, nil
		}

		return false, err
	}

	return true, nil
}

func (s *svc) resumeRecoverableChild(ctx context.Context, childID int64) error {
	return s.guardChildTransition(ctx, childID, func(guarded context.Context) error {
		link, err := s.links.GetLink(guarded, childID)
		if err != nil {
			return fmt.Errorf("reload recoverable child link: %w", err)
		}

		if link == nil || (link.State != subagent.StateStopped &&
			(link.State != subagent.StateError || link.DeliveredAt == 0)) {
			return nil
		}

		return s.resumeChildWithPendingInputLocked(guarded, childID)
	})
}

// resumeChild restarts a child's runner so its loop can finish.
func (s *svc) resumeChild(ctx context.Context, link subagent.Link) {
	rec, err := s.store.GetSession(ctx, link.ChildID)
	if err != nil {
		return
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return
	}

	if err := s.ensureRunner(ctx, link.ChildID, workDir, rec.ProjectID); err != nil {
		logger.Ctx(ctx).
			Named("daemon.sweep").
			Error("resume_child_failed", zap.Int64("child", link.ChildID), zap.Error(err))
	}
}
