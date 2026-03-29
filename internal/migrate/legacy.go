package migrate

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// legacyMigrations returns no-op placeholders for historical Go migrations 1–6.
// These existed as data-transformation migrations (ID remapping, column renames,
// FK enforcement) that have already been applied to all existing databases.
// New installs get the full schema from 00007_baseline.sql instead.
func legacyMigrations() []*goose.Migration {
	noop := &goose.GoFunc{RunDB: func(_ context.Context, _ *sql.DB) error { return nil }}

	return []*goose.Migration{
		goose.NewGoMigration(1, noop, nil),
		goose.NewGoMigration(2, noop, nil),
		goose.NewGoMigration(3, noop, nil),
		goose.NewGoMigration(4, noop, nil),
		goose.NewGoMigration(5, noop, nil),
		goose.NewGoMigration(6, noop, nil),
	}
}
