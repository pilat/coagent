package mcpstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

func (s *store) exists(ctx context.Context, projectID *int64, name string) bool {
	var found int

	var err error

	if projectID == nil {
		err = s.db.QueryRowContext(ctx,
			`SELECT 1 FROM mcp_servers WHERE name = ? AND project_id IS NULL LIMIT 1`, name).Scan(&found)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT 1 FROM mcp_servers WHERE name = ? AND project_id = ? LIMIT 1`, name, *projectID).Scan(&found)
	}

	return err == nil
}

// requireAffected turns a no-op write into a not-found error that says where the
// name does live, so "wrong scope" does not read as "never existed".
func (s *store) requireAffected(ctx context.Context, result sql.Result, projectID *int64, name string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows > 0 {
		return nil
	}

	return fmt.Errorf(
		"%w: %q in %s scope%s",
		ErrNotFound,
		name,
		scopeName(projectID),
		s.otherScopeHint(ctx, projectID, name),
	)
}

func (s *store) otherScopeHint(ctx context.Context, projectID *int64, name string) string {
	var found int

	if projectID == nil {
		if err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM mcp_servers WHERE name = ? AND project_id IS NOT NULL LIMIT 1`, name,
		).Scan(&found); err != nil {
			return ""
		}

		return " (it exists in project scope)"
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM mcp_servers WHERE name = ? AND project_id IS NULL LIMIT 1`, name,
	).Scan(&found); err != nil {
		return ""
	}

	return " (it exists in global scope)"
}

func (s *store) query(ctx context.Context, sqlText string, args ...any) ([]ServerDef, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("query mcp servers: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var defs []ServerDef

	for rows.Next() {
		var (
			def         ServerDef
			argsJSON    string
			envJSON     string
			enabledFlag int
		)

		if err := rows.Scan(&def.Name, &def.Command, &argsJSON, &envJSON, &enabledFlag); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}

		if err := def.decode(argsJSON, envJSON); err != nil {
			return nil, err
		}

		def.Enabled = enabledFlag != 0
		defs = append(defs, def)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mcp servers: %w", err)
	}

	return defs, nil
}

func (d *ServerDef) encode() (string, string, error) {
	args := d.Args
	if args == nil {
		args = []string{}
	}

	env := d.Env
	if env == nil {
		env = map[string]string{}
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return "", "", fmt.Errorf("marshal args: %w", err)
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", "", fmt.Errorf("marshal env: %w", err)
	}

	return string(argsJSON), string(envJSON), nil
}

func (d *ServerDef) decode(argsJSON, envJSON string) error {
	if err := json.Unmarshal([]byte(argsJSON), &d.Args); err != nil {
		return fmt.Errorf("unmarshal args for %q: %w", d.Name, err)
	}

	if err := json.Unmarshal([]byte(envJSON), &d.Env); err != nil {
		return fmt.Errorf("unmarshal env for %q: %w", d.Name, err)
	}

	return nil
}

func scopeName(projectID *int64) string {
	if projectID == nil {
		return "global"
	}

	return "project"
}

func boolToInt(v bool) int {
	if v {
		return 1
	}

	return 0
}
