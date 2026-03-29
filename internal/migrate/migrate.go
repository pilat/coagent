package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
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
	db, err := OpenDB(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := Run(ctx, db, ""); err != nil {
		_ = db.Close() // cleanup
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

// OpenDB opens (or creates) the SQLite database at dbPath.
// If dbPath is empty, uses ~/.coagent/daemon.db.
func OpenDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		globalDir, err := coagenthome.Dir()
		if err != nil {
			return nil, fmt.Errorf("user home dir: %w", err)
		}

		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			return nil, fmt.Errorf("create config dir: %w", err)
		}

		dbPath = filepath.Join(globalDir, coagenthome.DBFileName)
	}

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

	if dbPath != "" {
		pending, err := provider.HasPending(ctx)
		if err == nil && pending {
			backupDB(dbPath)
		}
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("goose up: %w", err)
	}

	log := logger.Named("migrate.run")
	for _, r := range results {
		log.Info("applied", zap.Int64("version", r.Source.Version), zap.Duration("duration", r.Duration))
	}

	return nil
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

func backupDB(dbPath string) {
	log := logger.Named("migrate.run")
	bakPath := dbPath + ".bak"

	src, err := os.Open(dbPath)
	if err != nil {
		log.Warn("backup_open_failed", zap.String("path", dbPath), zap.Error(err))
		return
	}
	defer src.Close()

	dst, err := os.Create(bakPath)
	if err != nil {
		log.Warn("backup_create_failed", zap.String("path", bakPath), zap.Error(err))
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		log.Warn("backup_copy_failed", zap.Error(err))
		return
	}

	log.Info("backup_created", zap.String("path", bakPath))
}
