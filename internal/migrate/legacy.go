package migrate

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// legacyMigrations returns no-op placeholders for historical Go migrations 1–6
// plus the data transformations that outgrew SQL. New installs still run the
// Go migrations so upgraded and fresh databases converge on one schema.
func legacyMigrations() []*goose.Migration {
	noop := &goose.GoFunc{RunDB: func(_ context.Context, _ *sql.DB) error { return nil }}

	return []*goose.Migration{
		goose.NewGoMigration(1, noop, nil),
		goose.NewGoMigration(2, noop, nil),
		goose.NewGoMigration(3, noop, nil),
		goose.NewGoMigration(4, noop, nil),
		goose.NewGoMigration(5, noop, nil),
		goose.NewGoMigration(6, noop, nil),
		managerOutboxBackfillMigration(),
		managementProjectMigration(),
	}
}
