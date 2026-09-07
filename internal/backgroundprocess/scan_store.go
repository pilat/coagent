package backgroundprocess

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *store) list(ctx context.Context, query string, args ...any) ([]Process, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list background processes: %w", err)
	}
	defer rows.Close()

	var processes []Process

	for rows.Next() {
		process, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}

		processes = append(processes, process)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list background processes: %w", err)
	}

	return processes, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProcess(row rowScanner) (Process, error) {
	var process Process
	var hostIntent string
	var state string
	var deliveryState string
	var targetSessionID sql.NullInt64

	err := row.Scan(
		&process.ID,
		&process.SessionID,
		&process.RootSessionID,
		&process.ToolCallID,
		&process.OutputPath,
		&process.Deadline,
		&process.CreatedAt,
		&process.AdvertisedAt,
		&process.OutputSize,
		&process.ExitCode,
		&hostIntent,
		&state,
		&process.FinishedAt,
		&deliveryState,
		&targetSessionID,
		&process.DeliveredAt,
	)
	if err != nil {
		return Process{}, fmt.Errorf("scan background process row: %w", err)
	}

	if targetSessionID.Valid {
		process.DeliveryTargetSessionID = targetSessionID.Int64
	}

	process.HostIntent = HostIntent(hostIntent)
	process.State = State(state)
	process.DeliveryState = deliveryState

	return process, nil
}
