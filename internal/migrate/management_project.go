package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/pressly/goose/v3"

	"github.com/pilat/coagent/internal/coagenthome"
)

func managementProjectMigration() *goose.Migration {
	return goose.NewGoMigration(40, &goose.GoFunc{RunDB: migrateManagementProject}, nil)
}

// migrateManagementProject adds the discovery-only projects.hidden flag and
// deletes the old CLI-owned sys:coagent graph. The physical
// <projects_root>/sys_coagent directory is preserved: Telegram managers reuse
// it for the new hidden ordinary project. Process-artifact directories are
// removed only for the exact deleted project IDs, never by broad target.
func migrateManagementProject(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin management project migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(
		ctx,
		`ALTER TABLE projects ADD COLUMN hidden BOOLEAN NOT NULL DEFAULT FALSE`,
	); err != nil {
		if !isDuplicateColumnError(err) {
			return fmt.Errorf("add projects hidden flag: %w", err)
		}
	}

	oldIDs, err := oldCLIProjectIDs(ctx, tx)
	if err != nil {
		return err
	}

	if err := deleteCLIProjectGraph(ctx, tx, oldIDs); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit management project migration: %w", err)
	}

	// Filesystem cleanup runs after the commit so a failed removal does not
	// roll back the durable delete; the rerun deletes the same exact targets.
	return removeProcessArtifactDirs(oldIDs)
}

func isDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// oldCLIProjectIDs records the exact project rows owned by the removed CLI
// protocol: the logical sys:coagent identity. Ordinary projects are untouched.
func oldCLIProjectIDs(ctx context.Context, tx *sql.Tx) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM projects WHERE name = 'sys:coagent'`)
	if err != nil {
		return nil, fmt.Errorf("list old cli projects: %w", err)
	}
	defer rows.Close()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan old cli project: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate old cli projects: %w", err)
	}

	return ids, nil
}

// deleteCLIProjectGraph removes every durable row of the old CLI session
// trees in foreign-key-safe order: children before parents, projects last.
func deleteCLIProjectGraph(ctx context.Context, tx *sql.Tx, projectIDs []int64) error {
	if len(projectIDs) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(projectIDs))
	placeholders = strings.TrimSuffix(placeholders, ",")

	args := make([]any, len(projectIDs))
	for i, id := range projectIDs {
		args[i] = id
	}

	sessionIDs := fmt.Sprintf(`SELECT id FROM sessions WHERE project_id IN (%s)`, placeholders)

	statements := []struct {
		label string
		query string
		args  []any
	}{
		{"messages", fmt.Sprintf(`DELETE FROM messages WHERE session_id IN (%s)`, sessionIDs), args},
		{"schedules", fmt.Sprintf(`DELETE FROM schedules WHERE session_id IN (%s)`, sessionIDs), args},
		{
			"tool activations",
			fmt.Sprintf(`DELETE FROM session_tool_activations WHERE session_id IN (%s)`, sessionIDs),
			args,
		},
		{"session inbox", fmt.Sprintf(`DELETE FROM session_inbox WHERE session_id IN (%s)`, sessionIDs), args},
		{"session outbox", fmt.Sprintf(`DELETE FROM session_outbox WHERE session_id IN (%s)`, sessionIDs), args},
		{
			"session deliveries",
			fmt.Sprintf(`DELETE FROM session_deliveries WHERE session_id IN (%s)`, sessionIDs),
			args,
		},
		{"budgets", fmt.Sprintf(`DELETE FROM session_budgets WHERE root_session_id IN (%s)`, sessionIDs), args},
		{"file reads", fmt.Sprintf(`DELETE FROM session_file_reads WHERE session_id IN (%s)`, sessionIDs), args},
		{"background processes", fmt.Sprintf(
			`DELETE FROM background_processes WHERE session_id IN (%s) OR root_session_id IN (%s)`,
			sessionIDs, sessionIDs), append(slices.Clip(args), args...)},
		{"subagent links", fmt.Sprintf(
			`DELETE FROM subagent_links WHERE parent_id IN (%s) OR child_id IN (%s)`,
			sessionIDs, sessionIDs), append(slices.Clip(args), args...)},
		{"memories", fmt.Sprintf(`DELETE FROM memories WHERE project_id IN (%s)`, placeholders), args},
		{"mcp servers", fmt.Sprintf(`DELETE FROM mcp_servers WHERE project_id IN (%s)`, placeholders), args},
		{"sessions", fmt.Sprintf(`DELETE FROM sessions WHERE id IN (%s)`, sessionIDs), args},
		{"projects", fmt.Sprintf(`DELETE FROM projects WHERE id IN (%s)`, placeholders), args},
		{"cli manager binding", `DELETE FROM manager_bindings WHERE manager_id = 'cli'`, nil},
	}

	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			return fmt.Errorf("delete %s: %w", stmt.label, err)
		}
	}

	return nil
}

// removeProcessArtifactDirs deletes only the exact process-artifact
// directories of removed projects. Missing directories are not an error, and
// an unresolvable home skips filesystem cleanup: the durable delete already
// committed, and orphaned artifact directories are inert without their rows.
func removeProcessArtifactDirs(projectIDs []int64) error {
	for _, id := range projectIDs {
		dir, err := coagenthome.ProcessProjectDir(id)
		if err != nil {
			continue
		}

		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove process artifact dir %s: %w", dir, err)
		}
	}

	return nil
}
