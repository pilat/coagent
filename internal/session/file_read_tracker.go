package session

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type fileReadTracker struct {
	store     sessionstore.FileReadStore
	sessionID int64
}

var _ builtin.FileReadTracker = (*fileReadTracker)(nil)

func (t *fileReadTracker) LookupRead(ctx context.Context, path string) (builtin.ReadRecord, bool, error) {
	if t == nil || t.store == nil {
		return builtin.ReadRecord{}, false, nil
	}

	record, found, err := t.store.LookupRead(ctx, t.sessionID, path)
	if err != nil {
		return builtin.ReadRecord{}, false, fmt.Errorf("lookup session file read: %w", err)
	}

	return builtin.ReadRecord{
		MtimeUnixNano: record.MtimeUnixNano,
		Size:          record.Size,
		Hash:          record.Hash,
	}, found, nil
}

func (t *fileReadTracker) RecordRead(ctx context.Context, path string, record builtin.ReadRecord) error {
	if t == nil || t.store == nil {
		return nil
	}

	if err := t.store.RecordRead(ctx, t.sessionID, path, sessionstore.FileReadRecord{
		MtimeUnixNano: record.MtimeUnixNano,
		Size:          record.Size,
		Hash:          record.Hash,
	}); err != nil {
		return fmt.Errorf("record session file read: %w", err)
	}

	return nil
}
