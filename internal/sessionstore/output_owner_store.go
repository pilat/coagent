package sessionstore

import (
	"context"
	"fmt"
)

func (s *Store) ListUnresolvedOutputOwners(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT json_extract(attributes, '$.manager_id')
		FROM session_outbox
		WHERE state <> 'delivered' AND json_type(attributes, '$.manager_id') = 'text'
		ORDER BY json_extract(attributes, '$.manager_id')`)
	if err != nil {
		return nil, fmt.Errorf("list unresolved output owners: %w", err)
	}
	defer rows.Close()

	owners := make([]string, 0)

	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("scan unresolved output owner: %w", err)
		}

		if owner != "" {
			owners = append(owners, owner)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unresolved output owners: %w", err)
	}

	return owners, nil
}
