package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// WorkReference identifies assistant-touched roots before expensive card enrichment.
type WorkReference struct {
	ID          int64
	Pending     bool
	LastFinalAt time.Time
}

// WorkReferences keeps final timestamps in one query so unread old sessions can be selected first.
func (s *store) WorkReferences(ctx context.Context, root int64) ([]WorkReference, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.session_id, w.pending, o.created_at FROM (
		SELECT session_id, MAX(observed_at IS NULL) AS pending FROM assistant_sends
		WHERE assistant_session_id = ? GROUP BY session_id
	) w LEFT JOIN session_outbox o ON o.id = (
		SELECT id FROM session_outbox WHERE session_id = w.session_id
		AND type = 'message_persistent' AND source_key LIKE 'message:%' ORDER BY id DESC LIMIT 1
	)`, root)
	if err != nil {
		return nil, fmt.Errorf("load assistant work references: %w", err)
	}
	defer rows.Close()
	var refs []WorkReference

	for rows.Next() {
		var ref WorkReference

		var at sql.NullTime
		if err := rows.Scan(&ref.ID, &ref.Pending, &at); err != nil {
			return nil, fmt.Errorf("scan assistant work reference: %w", err)
		}

		ref.LastFinalAt = at.Time
		refs = append(refs, ref)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant work references: %w", err)
	}

	return refs, nil
}
