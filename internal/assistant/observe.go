package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	answeredKind   = "answered"
	eventTextLimit = 20000
)

// window is one send's user-bounded answer window.
type window struct {
	state    sessionstore.InputState
	reason   string
	status   sessionstore.SessionStatus
	killed   bool
	nextUser int64
	answer   string
	settled  bool
}

// Observe settles the session's open sends whose answer window has closed, each with at most one event.
func (s *Store) Observe(ctx context.Context, sessionID int64, runErr error) error {
	sends, err := s.unobservedSends(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list assistant observations: %w", err)
	}

	if len(sends) == 0 {
		return nil
	}

	// The runtime's own yield verdict: a final written while this is true waits for that work.
	busy, err := s.sessions.HasBackgroundWakeSource(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("check assistant work wake source: %w", err)
	}

	var project string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(p.name, '') FROM sessions s
		LEFT JOIN projects p ON p.id = s.project_id WHERE s.id = ?`, sessionID).Scan(&project); err != nil {
		return fmt.Errorf("load observed project: %w", err)
	}

	for _, sd := range sends {
		if err := s.observeSend(ctx, sd, project, busy, runErr); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) observeSend(ctx context.Context, sd send, project string, busy bool, runErr error) error {
	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		w, err := observeWindowTx(ctx, tx, sd.inputID)
		if err != nil {
			return fmt.Errorf("observe assistant input: %w", err)
		}

		kind, text, settled := decide(w, busy, runErr)
		if !settled {
			return nil
		}

		won, err := markObservedTx(ctx, tx, sd.inputID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("settle assistant observation: %w", err)
		}

		if !won || kind == "" {
			return nil
		}

		if kind == answeredKind {
			text = fmt.Sprintf("%q", text)
		}

		body := fmt.Sprintf("← %s (%d) %s: %s", project, sd.sessionID, kind, text)
		in := eventInput(sd.root, sd.sessionID, kind, body)
		in.DeliveryKey = fmt.Sprintf("assistant:send:%d", sd.inputID)

		if _, err := sessionstore.EnqueueTx(ctx, tx, in); err != nil {
			return fmt.Errorf("enqueue assistant observation: %w", err)
		}

		return nil
	})
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		logger.Ctx(ctx).Named("assistant.observe").Warn("assistant observation deferred", zap.Error(err))
		return nil
	}

	if err != nil {
		return fmt.Errorf("commit assistant observation: %w", err)
	}

	return nil
}

// observeWindowTx ignores process and agent inputs when bounding the answer.
func observeWindowTx(ctx context.Context, tx *sql.Tx, inputID int64) (window, error) {
	var (
		w                         window
		sessionID, acceptedID     int64
		checkPending, lastIsFinal bool
	)

	if err := tx.QueryRowContext(ctx, `SELECT i.session_id, i.state, COALESCE(i.resolution_reason, ''),
			COALESCE(i.accepted_message_id, 0), COALESCE(s.status, ''), s.killed_at IS NOT NULL,
			s.completion_check_candidate_id IS NOT NULL
		FROM session_inbox i JOIN sessions s ON s.id = i.session_id WHERE i.id = ?`, inputID).
		Scan(&sessionID, &w.state, &w.reason, &acceptedID, &w.status, &w.killed, &checkPending); err != nil {
		return window{}, fmt.Errorf("load observed input: %w", err)
	}

	if w.state != sessionstore.InputStateAccepted {
		return w, nil
	}

	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(accepted_message_id), 0) FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'accepted' AND accepted_message_id > ?`,
		sessionID, acceptedID).Scan(&w.nextUser); err != nil {
		return window{}, fmt.Errorf("bound assistant answer: %w", err)
	}

	answer, err := windowAnswerTx(ctx, tx, sessionID, acceptedID, w.nextUser)
	if err != nil {
		return window{}, err
	}

	w.answer = answer

	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN `+finalSQL+` THEN 1 ELSE 0 END FROM messages m
		WHERE m.session_id = ? AND m.id > ? AND (? = 0 OR m.id <= ?)
			AND m.rejected_reason IS NULL AND m.compacted_at IS NULL
		ORDER BY m.position IS NULL DESC, m.position DESC, m.id DESC LIMIT 1`,
		sessionID, acceptedID, w.nextUser, w.nextUser).Scan(&lastIsFinal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return window{}, fmt.Errorf("load assistant answer window end: %w", err)
	}

	w.settled = lastIsFinal && !checkPending

	return w, nil
}

// windowAnswerTx joins every final in the window, oldest first: a closing final often
// only points back at an earlier one that holds the substance.
func windowAnswerTx(ctx context.Context, tx *sql.Tx, sessionID, afterID, upperID int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(m.content, '') FROM messages m
		WHERE m.session_id = ? AND m.id > ? AND (? = 0 OR m.id <= ?)
			AND m.rejected_reason IS NULL AND m.compacted_at IS NULL AND `+finalSQL+`
		ORDER BY m.position IS NULL, m.position, m.id`, sessionID, afterID, upperID, upperID)
	if err != nil {
		return "", fmt.Errorf("load assistant answer window: %w", err)
	}
	defer rows.Close()

	var finals []string

	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return "", fmt.Errorf("scan assistant answer: %w", err)
		}

		if text := strings.TrimSpace(content); !slices.Contains(finals, text) {
			finals = append(finals, text)
		}
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read assistant answer: %w", err)
	}

	return strings.Join(finals, "\n\n---\n\n"), nil
}

func decide(w window, busy bool, runErr error) (string, string, bool) {
	if w.state == sessionstore.InputStateCancelled || w.state == sessionstore.InputStateRejected {
		cause := string(sessionstore.SessionStatusStopped)
		if w.killed || strings.Contains(w.reason, "kill") {
			cause = string(sessionstore.SessionStatusKilled)
		}

		return failedStatus, cause, true
	}

	terminal := w.status == sessionstore.SessionStatusStopped ||
		w.status == sessionstore.SessionStatusKilled ||
		w.status == sessionstore.SessionStatusError
	if w.state == sessionstore.InputStatePending && !terminal {
		return "", "", false
	}

	if w.state == sessionstore.InputStatePending || terminal {
		cause := failureCause(w.status, runErr)
		if w.answer != "" {
			cause += "\n\nBefore that it said:\n" + w.answer
		}

		return failedStatus, cause, true
	}

	if busy {
		return "", "", false
	}

	if w.settled && w.answer != "" {
		return answeredKind, w.answer, true
	}

	if w.nextUser != 0 {
		return "", "", true
	}

	return "", "", false
}

func failureCause(status sessionstore.SessionStatus, runErr error) string {
	if status == sessionstore.SessionStatusStopped {
		return string(status)
	}

	if status == sessionstore.SessionStatusKilled {
		return string(status)
	}

	if runErr != nil {
		return logger.Redact(runErr.Error())
	}

	if status == sessionstore.SessionStatusError {
		return "session error"
	}

	return "ended without an answer"
}

func eventInput(root, target int64, kind, text string) sessionstore.Input {
	return sessionstore.Input{
		SessionID: root, Source: sessionstore.InputSourceAgent, Content: ClipMiddle(text, eventTextLimit),
		Attributes: map[string]any{"assistant_event": kind, "session_id": target},
	}
}
