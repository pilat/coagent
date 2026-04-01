package mcpstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicate reports a server that already exists in the target scope.
// ErrNotFound reports a name absent from the scope the caller addressed.
var (
	ErrDuplicate = errors.New("mcp server already exists")
	ErrNotFound  = errors.New("mcp server not found")
)

// ServerDef is one registry row. Env values are stored verbatim, references included.
type ServerDef struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Enabled bool
}

// Store is CRUD over the registry. A nil projectID addresses the global scope.
type Store interface {
	Add(ctx context.Context, projectID *int64, def ServerDef) error
	Remove(ctx context.Context, projectID *int64, name string) error
	SetEnabled(ctx context.Context, projectID *int64, name string, enabled bool) error

	// ListForProject returns the enabled set a session gets: globals overridden by
	// project rows. A disabled project row suppresses its global, never falls back.
	ListForProject(ctx context.Context, projectID int64) ([]ServerDef, error)

	// ListAll returns every row, disabled included, split by scope — what the
	// list tool renders.
	ListAll(ctx context.Context, projectID int64) (globals, project []ServerDef, err error)
}

var _ Store = (*store)(nil)

type store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) Add(ctx context.Context, projectID *int64, def ServerDef) error {
	args, env, err := def.encode()
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	// The partial unique indexes decide, not a prior SELECT: check-then-act would
	// hand a concurrent second add a raw constraint error instead of ErrDuplicate.
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO mcp_servers (project_id, name, command, args, env, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, def.Name, def.Command, args, env, boolToInt(def.Enabled), now, now,
	)
	if err == nil {
		return nil
	}

	if s.exists(ctx, projectID, def.Name) {
		return fmt.Errorf("%w: %q in %s scope", ErrDuplicate, def.Name, scopeName(projectID))
	}

	return fmt.Errorf("insert mcp server: %w", err)
}

func (s *store) Remove(ctx context.Context, projectID *int64, name string) error {
	var (
		result sql.Result
		err    error
	)

	if projectID == nil {
		result, err = s.db.ExecContext(ctx,
			`DELETE FROM mcp_servers WHERE name = ? AND project_id IS NULL`, name)
	} else {
		result, err = s.db.ExecContext(ctx,
			`DELETE FROM mcp_servers WHERE name = ? AND project_id = ?`, name, *projectID)
	}

	if err != nil {
		return fmt.Errorf("delete mcp server: %w", err)
	}

	return s.requireAffected(ctx, result, projectID, name)
}

func (s *store) SetEnabled(ctx context.Context, projectID *int64, name string, enabled bool) error {
	var (
		result sql.Result
		err    error
	)

	now := time.Now().UTC()

	if projectID == nil {
		result, err = s.db.ExecContext(ctx,
			`UPDATE mcp_servers SET enabled = ?, updated_at = ? WHERE name = ? AND project_id IS NULL`,
			boolToInt(enabled), now, name)
	} else {
		result, err = s.db.ExecContext(ctx,
			`UPDATE mcp_servers SET enabled = ?, updated_at = ? WHERE name = ? AND project_id = ?`,
			boolToInt(enabled), now, name, *projectID)
	}

	if err != nil {
		return fmt.Errorf("update mcp server: %w", err)
	}

	return s.requireAffected(ctx, result, projectID, name)
}

func (s *store) ListForProject(ctx context.Context, projectID int64) ([]ServerDef, error) {
	globals, project, err := s.ListAll(ctx, projectID)
	if err != nil {
		return nil, err
	}

	shadowed := make(map[string]struct{}, len(project))

	var merged []ServerDef

	for _, def := range project {
		shadowed[def.Name] = struct{}{}

		if def.Enabled {
			merged = append(merged, def)
		}
	}

	for _, def := range globals {
		if _, taken := shadowed[def.Name]; taken || !def.Enabled {
			continue
		}

		merged = append(merged, def)
	}

	return merged, nil
}

func (s *store) ListAll(ctx context.Context, projectID int64) ([]ServerDef, []ServerDef, error) {
	globals, err := s.query(ctx, `SELECT name, command, args, env, enabled FROM mcp_servers
		WHERE project_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}

	project, err := s.query(ctx, `SELECT name, command, args, env, enabled FROM mcp_servers
		WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, nil, err
	}

	return globals, project, nil
}
