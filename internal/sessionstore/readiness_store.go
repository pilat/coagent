package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Readiness struct {
	SessionID     int64
	OutputID      int64
	ReleasesInput bool
	SessionStatus SessionStatus
	Ready         bool
	Reason        string
}

type ReadinessStore interface {
	OutputReadiness(ctx context.Context, outputID int64) (*Readiness, error)
	LatestReleasingOutputID(ctx context.Context, sessionID int64) (int64, error)
}

var _ ReadinessStore = (*store)(nil)

//nolint:wsl_v5 // Readiness derives one result from the joined durable row.
func (s *store) OutputReadiness(ctx context.Context, outputID int64) (*Readiness, error) {
	// One read transaction keeps the row lookup and the newest-releasing check
	// on the same snapshot: a releasing row committing between them must not
	// flash readiness for a row that just lost the crown.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin readiness read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var readiness Readiness
	var releases bool
	var outputState OutputState
	var sessionStatus SessionStatus
	var sourceKey string

	err = tx.QueryRowContext(ctx, `SELECT session_outbox.session_id, session_outbox.id,
		session_outbox.releases_input, session_outbox.state, sessions.status,
		COALESCE(session_outbox.source_key, '')
		FROM session_outbox JOIN sessions ON sessions.id = session_outbox.session_id
		WHERE session_outbox.id = ?`, outputID).Scan(&readiness.SessionID, &readiness.OutputID,
		&releases, &outputState, &sessionStatus, &sourceKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOutput
	}

	if err != nil {
		return nil, fmt.Errorf("load output readiness: %w", err)
	}

	if !releases || outputState != OutputStateDelivered {
		return &readiness, nil
	}
	readiness.ReleasesInput = releases
	readiness.SessionStatus = sessionStatus

	// Readiness belongs only to the newest releasing obligation: acknowledging an
	// older final ahead of a queued stop completion must stay silent.
	var newest int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM session_outbox
		WHERE session_id = ? AND releases_input = 1`, readiness.SessionID).Scan(&newest); err != nil {
		return nil, fmt.Errorf("load newest releasing output: %w", err)
	}

	if newest != outputID {
		return &readiness, nil
	}

	if len(sourceKey) >= len("budget:") && sourceKey[:len("budget:")] == "budget:" {
		var phase string
		if err := tx.QueryRowContext(ctx, `SELECT park_phase FROM session_budgets
			WHERE root_session_id = ?`, readiness.SessionID).Scan(&phase); err != nil {
			return nil, fmt.Errorf("load budget readiness: %w", err)
		}

		readiness.Ready = phase == "parked" && sessionStatus == SessionStatusStopped
		readiness.Reason = "budget reached"

		return &readiness, nil
	}

	switch sessionStatus {
	case SessionStatusActive, SessionStatusCompleted:
		readiness.Ready = true
	case SessionStatusSuspended, SessionStatusStopping, SessionStatusTerminating:
	case SessionStatusError:
		readiness.Ready = true
		readiness.Reason = string(SessionStatusError)
	case SessionStatusStopped:
		readiness.Ready = true
		readiness.Reason = "stopped"
	case SessionStatusKilled:
		readiness.Ready = true
		readiness.Reason = "killed"
	}

	return &readiness, nil
}

func (s *store) LatestReleasingOutputID(ctx context.Context, sessionID int64) (int64, error) {
	var id int64

	err := s.db.QueryRowContext(ctx, `SELECT id FROM session_outbox
		WHERE session_id = ? AND releases_input = 1 ORDER BY id DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoOutput
	}

	if err != nil {
		return 0, fmt.Errorf("load latest releasing output: %w", err)
	}

	return id, nil
}
