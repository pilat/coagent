package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
	_ "modernc.org/sqlite" // register sqlite3 driver

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/migrations"
)

// Open opens the default database and runs migrations. Caller owns Close.
func Open(ctx context.Context) (*sql.DB, error) {
	dbPath, existed, err := productionDBPath()
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	db, err := OpenDB(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := run(ctx, db, dbPath, existed); err != nil {
		_ = db.Close() // cleanup
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

// OpenDB opens (or creates) the SQLite database at dbPath.
// If dbPath is empty, uses ~/.coagent/daemon.db.
func OpenDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	resolvedPath, err := resolveDBPath(dbPath)
	if err != nil {
		return nil, err
	}

	dbPath = resolvedPath

	// Apply connection-scoped settings via the DSN so EVERY pooled connection gets
	// them. Every explicit transaction in the repository writes; immediate mode
	// reserves its writer slot before any reads, avoiding an impossible deferred
	// read-snapshot -> writer upgrade when parent and child sessions commit at the
	// same time. busy_timeout then waits for the current writer instead of failing.
	dsn := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
		escapeURIPath(dbPath),
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	if err := configurePragmas(ctx, db); err != nil {
		_ = db.Close() // cleanup
		return nil, err
	}

	return db, nil
}

// Run applies all pending migrations (legacy Go no-ops + SQL files).
func Run(ctx context.Context, db *sql.DB, dbPath string) error {
	return run(ctx, db, dbPath, false)
}

func run(ctx context.Context, db *sql.DB, dbPath string, backup bool) error {
	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations.FS,
		goose.WithGoMigrations(legacyMigrations()...),
		goose.WithLogger(goose.NopLogger()),
	)
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}

	if backup {
		pending, err := provider.HasPending(ctx)
		if err != nil {
			return fmt.Errorf("check pending migrations: %w", err)
		}

		if pending {
			if err := backupDB(ctx, db, dbPath); err != nil {
				return fmt.Errorf("backup database: %w", err)
			}
		}
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	log := logger.Ctx(ctx).Named("migrate.run")
	for _, r := range results {
		log.Info("applied", zap.Int64("version", r.Source.Version), zap.Duration("duration", r.Duration))
	}

	return nil
}

func resolveDBPath(dbPath string) (string, error) {
	if dbPath != "" {
		return dbPath, nil
	}

	globalDir, err := coagenthome.Dir()
	if err != nil {
		return "", fmt.Errorf("user home dir: %w", err)
	}

	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}

	return filepath.Join(globalDir, coagenthome.DBFileName), nil
}

func productionDBPath() (string, bool, error) {
	dbPath, err := resolveDBPath("")
	if err != nil {
		return "", false, err
	}

	_, err = os.Stat(dbPath)
	if errors.Is(err, os.ErrNotExist) {
		return dbPath, false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("inspect database: %w", err)
	}

	return dbPath, true, nil
}

// escapeURIPath percent-encodes the characters SQLite's URI parser treats as
// delimiters; a raw '#' or '?' in dbPath silently opens a different file.
func escapeURIPath(dbPath string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(dbPath)
}

func configurePragmas(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}

	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}

	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	return nil
}
