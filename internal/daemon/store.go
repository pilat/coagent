package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

// Store persists daemon state (project registry) in SQLite.
type Store interface {
	GetOrCreateProject(ctx context.Context, workDir string) (int64, error)
	GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error)
	GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error)
	GetProjectWorkDir(ctx context.Context, projectID int64) (string, error)
	GetProjectName(ctx context.Context, projectID int64) (string, error)
	ListProjects(ctx context.Context) ([]ProjectRow, error)
}

// ProjectRow is a project registry row.
type ProjectRow struct {
	ID      int64
	Name    string
	WorkDir string
	Hidden  bool
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
	if name == controllerapi.CoagentManagementProjectDir || strings.ContainsRune(name, ':') {
		return 0, fmt.Errorf("project directory name %q is reserved", name)
	}

	return s.getOrCreateProject(ctx, absPath, name)
}

// GetOrCreateNamedProject registers workDir under an explicit display name that
// need not equal the directory basename. /gwt uses it so a worktree whose leaf is
// a bare branch name reads as "<repo>/<branch>". The name is display-only; ':'
// stays rejected so display names never collide with directory-derived identity.
func (s *store) GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error) {
	absPath, err := filepath.Abs(workDir)
	if err != nil {
		absPath = workDir
	}

	if name == "" || strings.ContainsRune(name, ':') {
		return 0, fmt.Errorf("invalid project name %q", name)
	}

	return s.getOrCreateProject(ctx, absPath, name)
}

func (s *store) GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error) {
	absPath, err := filepath.Abs(workDir)
	if err != nil {
		absPath = workDir
	}

	name := filepath.Base(absPath)

	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO projects (work_dir, name, hidden) VALUES (?, ?, TRUE)
			ON CONFLICT(work_dir) DO UPDATE SET hidden = excluded.hidden`,
		absPath,
		name,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert hidden project: %w", err)
	}

	var projectID int64

	err = s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE work_dir = ?`, absPath).Scan(&projectID)
	if err != nil {
		return 0, fmt.Errorf("select project: %w", err)
	}

	return projectID, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, work_dir, hidden FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectRow

	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.WorkDir, &p.Hidden); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}

		projects = append(projects, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}

	return projects, nil
}

// DB exposes the underlying database for the background-process ledger.
func (s *store) DB() *sql.DB {
	return s.db
}

func (s *store) getOrCreateProject(ctx context.Context, absPath, name string) (int64, error) {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO projects (work_dir, name) VALUES (?, ?)`,
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
