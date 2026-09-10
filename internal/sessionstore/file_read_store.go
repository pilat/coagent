package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FileReadRecord is the fingerprint recorded after a session observed a file.
type FileReadRecord struct {
	MtimeUnixNano int64
	Size          int64
	Hash          string
}

// FileReadStore persists fingerprints of files observed by a session.
type FileReadStore interface {
	LookupRead(ctx context.Context, sessionID int64, path string) (FileReadRecord, bool, error)
	RecordRead(ctx context.Context, sessionID int64, path string, record FileReadRecord) error
}

var _ FileReadStore = (*store)(nil)

//nolint:wsl_v5 // Lookup keeps query, not-found handling, and scan errors together.
func (s *store) LookupRead(ctx context.Context, sessionID int64, path string) (FileReadRecord, bool, error) {
	var record FileReadRecord
	err := s.db.QueryRowContext(ctx, `SELECT mtime_unix_nano, size, hash
		FROM session_file_reads WHERE session_id = ? AND path = ?`, sessionID, path).
		Scan(&record.MtimeUnixNano, &record.Size, &record.Hash)
	if err == sql.ErrNoRows {
		return FileReadRecord{}, false, nil
	}
	if err != nil {
		return FileReadRecord{}, false, fmt.Errorf("lookup file read: %w", err)
	}

	return record, true, nil
}

func (s *store) RecordRead(ctx context.Context, sessionID int64, path string, record FileReadRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_file_reads
		(session_id, path, mtime_unix_nano, size, hash, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, path) DO UPDATE SET
		mtime_unix_nano = excluded.mtime_unix_nano,
		size = excluded.size,
		hash = excluded.hash,
		updated_at = excluded.updated_at`,
		sessionID, path, record.MtimeUnixNano, record.Size, record.Hash, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("record file read: %w", err)
	}

	return nil
}
