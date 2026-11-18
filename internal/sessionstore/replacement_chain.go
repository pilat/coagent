package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

func (s *Store) ResolveReplacement(ctx context.Context, sessionID int64, managerID string) (int64, error) {
	_, record, err := s.forwardReplacements(ctx, sessionID, managerID)
	if err != nil {
		return 0, err
	}

	if record.Status != SessionStatusTerminating && record.KilledAt == nil {
		return record.ID, nil
	}

	return 0, ErrSessionNotAcceptingInput
}

// ReplacementChain includes valid predecessors and permits a killed final root.
func (s *Store) ReplacementChain(ctx context.Context, sessionID int64, managerID string) ([]int64, error) {
	chain, _, err := s.forwardReplacements(ctx, sessionID, managerID)
	if err != nil {
		return nil, err
	}

	record, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	var predecessors []int64

	for range 16 {
		var previous any

		err := s.db.QueryRowContext(ctx, `SELECT json_extract(attributes, '$.old_session_id')
			FROM session_outbox WHERE session_id = ? AND type = 'session_replaced'
			ORDER BY id DESC LIMIT 1`, record.ID).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("load session predecessor: %w", err)
		}

		previousID, valid := previous.(int64)
		if !valid || previousID <= 0 {
			break
		}

		if slices.Contains(chain, previousID) || slices.Contains(predecessors, previousID) {
			break
		}

		prior, err := s.GetSession(ctx, previousID)
		if errors.Is(err, errSessionNotFound) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("load predecessor session: %w", err)
		}

		if prior.ParentID != 0 || prior.ProjectID != record.ProjectID ||
			prior.Attributes[managerIDAttribute] != managerID {
			break
		}

		predecessors = append(predecessors, prior.ID)
		record = prior
	}

	slices.Reverse(predecessors)

	return append(predecessors, chain...), nil
}

func (s *Store) forwardReplacements(
	ctx context.Context,
	sessionID int64,
	managerID string,
) ([]int64, *SessionRecord, error) {
	seen := make(map[int64]struct{})
	var chain []int64

	for range 16 {
		if _, duplicate := seen[sessionID]; duplicate {
			return nil, nil, errors.New("session replacement cycle")
		}

		seen[sessionID] = struct{}{}

		record, err := s.GetSession(ctx, sessionID)
		if err != nil {
			return nil, nil, err
		}

		owner, _ := record.Attributes[managerIDAttribute].(string)
		if owner != managerID {
			return nil, nil, ErrOutputOwner
		}

		chain = append(chain, sessionID)
		if record.Status != SessionStatusTerminating && record.KilledAt == nil {
			return chain, record, nil
		}

		var replacementID int64

		err = s.db.QueryRowContext(ctx, `
			SELECT session_id FROM session_outbox
			WHERE type = 'session_replaced'
				AND json_extract(attributes, '$.old_session_id') = ?
				AND json_extract(attributes, '$.manager_id') = ?
			ORDER BY id DESC LIMIT 1`, sessionID, managerID).Scan(&replacementID)
		if errors.Is(err, sql.ErrNoRows) {
			return chain, record, nil
		}

		if err != nil {
			return nil, nil, fmt.Errorf("load replacement session: %w", err)
		}

		next, err := s.GetSession(ctx, replacementID)
		if err != nil {
			return nil, nil, err
		}

		nextOwner, _ := next.Attributes[managerIDAttribute].(string)
		if next.ParentID != 0 || next.ProjectID != record.ProjectID || nextOwner != managerID {
			return nil, nil, errors.New("invalid replacement session")
		}

		sessionID = replacementID
	}

	return nil, nil, errors.New("session replacement chain too deep")
}
