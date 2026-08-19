package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

// Store persists daemon state (project registry) in SQLite.
type Store interface {
	GetOrCreateProject(ctx context.Context, workDir string) (int64, error)
	GetOrCreateSystemProject(ctx context.Context, workDir, name string) (int64, error)
	GetProjectWorkDir(ctx context.Context, projectID int64) (string, error)
	GetProjectName(ctx context.Context, projectID int64) (string, error)
	ListProjects(ctx context.Context) ([]ProjectRow, error)
}

// ProjectRow is a project registry row.
type ProjectRow struct {
	ID      int64
	Name    string
	WorkDir string
}

var _ Store = (*store)(nil)

type store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) GetOrCreateProject(ctx context.Context, workDir string) (int64, error) {
	absPath, err := filepath.Abs(workDir)
	if err != nil {
		absPath = workDir
	}

	name := filepath.Base(absPath)
	if name == controllerapi.CoagentSystemProjectDir || strings.ContainsRune(name, ':') {
		return 0, fmt.Errorf("project directory name %q is reserved", name)
	}

	return s.getOrCreateProject(ctx, absPath, name, false)
}

func (s *store) GetOrCreateSystemProject(ctx context.Context, workDir, name string) (int64, error) {
	absPath, err := filepath.Abs(workDir)
	if err != nil {
		absPath = workDir
	}

	if name != controllerapi.CoagentSystemProjectName ||
		filepath.Base(absPath) != controllerapi.CoagentSystemProjectDir {
		return 0, errors.New("unknown system project identity")
	}

	return s.getOrCreateProject(ctx, absPath, name, true)
}

func (s *store) GetProjectName(ctx context.Context, projectID int64) (string, error) {
	var name string

	err := s.db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, projectID).Scan(&name)
	if err != nil {
		return "", fmt.Errorf("project %d not found: %w", projectID, err)
	}

	return name, nil
}

func (s *store) GetProjectWorkDir(ctx context.Context, projectID int64) (string, error) {
	var workDir string

	err := s.db.QueryRowContext(ctx, `SELECT work_dir FROM projects WHERE id = ?`, projectID).Scan(&workDir)
	if err != nil {
		return "", fmt.Errorf("project %d not found: %w", projectID, err)
	}

	return workDir, nil
}

// ListProjects returns every project. Root-prefix filtering is done by the caller
// in Go, not via SQL LIKE — an underscore in a home path is a LIKE wildcard.
func (s *store) ListProjects(ctx context.Context) ([]ProjectRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, work_dir FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectRow

	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.WorkDir); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}

		projects = append(projects, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}

	return projects, nil
}

func (s *store) getOrCreateProject(ctx context.Context, absPath, name string, system bool) (int64, error) {
	query := `INSERT OR IGNORE INTO projects (work_dir, name) VALUES (?, ?)`
	if system {
		query = `INSERT INTO projects (work_dir, name) VALUES (?, ?)
			ON CONFLICT(work_dir) DO UPDATE SET name = excluded.name`
	}

	_, err := s.db.ExecContext(
		ctx,
		query,
		absPath,
		name,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert project: %w", err)
	}

	var projectID int64

	err = s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE work_dir = ?`, absPath).Scan(&projectID)
	if err != nil {
		return 0, fmt.Errorf("select project: %w", err)
	}

	return projectID, nil
}
