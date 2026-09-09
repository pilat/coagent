package backgroundprocess

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

func (s *store) finalizeRunning(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	outcome State,
	exitCode *int,
	outputSize int64,
	fallbackIntent HostIntent,
) (Process, bool, error) {
	recordedExit := exitCode
	if outcome != StateCompleted && outcome != StateFailed {
		recordedExit = nil
	}

	now := time.Now().UTC()

	res, err := tx.ExecContext(ctx, `
		UPDATE background_processes
		SET state = ?, exit_code = ?, output_size = ?, finished_at = ?,
			host_intent = CASE WHEN host_intent = '' AND ? <> '' THEN ? ELSE host_intent END
		WHERE id = ? AND state = 'running'`,
		string(outcome), recordedExit, outputSize, now, string(fallbackIntent), string(fallbackIntent),
		id,
	)
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if rows != 1 {
		return Process{}, false, nil
	}

	winner, err := scanProcess(tx.QueryRowContext(ctx,
		`SELECT `+processColumns+` FROM background_processes WHERE id = ?`, id,
	))
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if winner.AdvertisedAt != nil && !winner.WakeSuppressed() {
		if err := insertCompletionInbox(ctx, tx, winner); err != nil {
			return Process{}, false, err
		}
	}

	return winner, true, nil
}

func insertCompletionInbox(ctx context.Context, tx *sql.Tx, process Process) error {
	var status string

	err := tx.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ? AND killed_at IS NULL`, process.SessionID).
		Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || status == "terminating" || status == "killed" {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load process input session: %w", err)
	}

	attributes, err := json.Marshal(map[string]any{"process_id": process.ID})
	if err != nil {
		return fmt.Errorf("encode process input attributes: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO session_inbox (session_id, source, raw_content, attributes, received_at)
		VALUES (?, 'process', ?, ?, ?)`, process.SessionID, formatCompletion(process), string(attributes), *process.FinishedAt)
	if err != nil {
		return fmt.Errorf("insert process completion input: %w", err)
	}

	return nil
}

func formatCompletion(process Process) string {
	exitCode := "unavailable"
	if process.ExitCode != nil {
		exitCode = strconv.Itoa(*process.ExitCode)
	}

	preview, _, readable := ExtractTail(process.OutputPath, TailPreviewLines, TailPreviewBytes)

	status := "empty"
	if !readable || !ValidEventTail(preview) {
		status = "binary_omitted"
		preview = ""
	} else if preview != "" {
		status = "text"
		preview = TruncateTailToBytes(preview, TailPreviewBytes)
	}

	duration := process.FinishedAt.Sub(process.CreatedAt).String()

	lines := []string{
		"<process_completion>", "process_id: " + html.EscapeString(process.ID),
		"state: " + html.EscapeString(string(process.State)), "exit_code: " + exitCode,
		"duration: " + duration, "origin_session_id: " + strconv.FormatInt(process.SessionID, 10),
		"output_file: " + html.EscapeString(process.OutputPath), "preview_status: " + status,
	}
	if status == "text" {
		lines = append(lines, "final_output_preview:", html.EscapeString(preview))
	}

	return strings.Join(append(lines, "</process_completion>"), "\n")
}
