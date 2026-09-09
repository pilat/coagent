package backgroundprocess

import (
	"context"
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
	)
	if err != nil {
		return Process{}, fmt.Errorf("scan background process row: %w", err)
	}

	process.HostIntent = HostIntent(hostIntent)
	process.State = State(state)

	return process, nil
}
