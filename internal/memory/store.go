package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type CuratedMemory struct {
	ID        int64
	ProjectID int64
	Text      string
	CreatedAt time.Time
}

type CuratedStore interface {
	SaveMemory(ctx context.Context, projectID int64, text string) (int64, error)
	DeleteMemory(ctx context.Context, projectID, id int64) error
	ListMemoryTexts(ctx context.Context, projectID int64) ([]MemoryEntry, error)
	CountMemories(ctx context.Context, projectID int64) (int, error)
	ListMemories(ctx context.Context, projectID int64) ([]CuratedMemory, error)
}

var _ CuratedStore = (*curatedStore)(nil)

type curatedStore struct {
	db *sql.DB
}

func NewCuratedStore(db *sql.DB) CuratedStore {
	return &curatedStore{db: db}
}

func (s *curatedStore) SaveMemory(ctx context.Context, projectID int64, text string) (int64, error) {
	result, err := s.db.ExecContext(
		ctx,
		`INSERT INTO memories (project_id, text) VALUES (?, ?)`,
		projectID, text,
	)
	if err != nil {
		return 0, fmt.Errorf("insert memory: %w", err)
	}

	newID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	return newID, nil
}

// DeleteMemory removes a memory owned by projectID; an id belonging to another
// project is indistinguishable from a missing one.
func (s *curatedStore) DeleteMemory(ctx context.Context, projectID, memID int64) error {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM memories WHERE id = ? AND project_id = ?`,
		memID, projectID,
	)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("memory %d not found", memID)
	}

	return nil
}

func (s *curatedStore) ListMemories(ctx context.Context, projectID int64) ([]CuratedMemory, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, project_id, text, created_at FROM memories WHERE project_id = ? ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer rows.Close()

	var memories []CuratedMemory

	for rows.Next() {
		var m CuratedMemory
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}

		memories = append(memories, m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories: %w", err)
	}

	return memories, nil
}

func (s *curatedStore) ListMemoryTexts(ctx context.Context, projectID int64) ([]MemoryEntry, error) {
	memories, err := s.ListMemories(ctx, projectID)
	if err != nil {
		return nil, err
	}

	entries := make([]MemoryEntry, len(memories))

	for i, m := range memories {
		entries[i] = MemoryEntry{ID: m.ID, Text: m.Text}
	}

	return entries, nil
}

func (s *curatedStore) CountMemories(ctx context.Context, projectID int64) (int, error) {
	var count int

	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM memories WHERE project_id = ?`, projectID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count memories: %w", err)
	}

	return count, nil
}
