package migrate

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

// legacyMigrations keeps placeholders for versions 1–6 and squashed 48–49, plus data transformations.
// Fresh installs and upgraded databases retain the same version sequence.
func legacyMigrations() []*goose.Migration {
	noop := &goose.GoFunc{RunDB: func(_ context.Context, _ *sql.DB) error { return nil }}

	return []*goose.Migration{
		goose.NewGoMigration(1, noop, nil),
		goose.NewGoMigration(2, noop, nil),
		goose.NewGoMigration(3, noop, nil),
		goose.NewGoMigration(4, noop, nil),
		goose.NewGoMigration(5, noop, nil),
		goose.NewGoMigration(6, noop, nil),
		goose.NewGoMigration(48, noop, nil),
		goose.NewGoMigration(49, noop, nil),
		managerOutboxBackfillMigration(),
		managementProjectMigration(),
	}
}
