package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestOpenFreshDefaultDatabaseDoesNotCreateBackup(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	t.Cleanup(restore)

	db, err := Open(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	dbPath := filepath.Join(home, coagenthome.DirName, coagenthome.DBFileName)
	_, err = os.Stat(dbPath + ".bak")
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, int64(23), currentVersion(t, db))
}

func TestOpenBacksUpWALDatabaseBeforeMigration(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	t.Cleanup(restore)

	ctx := t.Context()
	dbPath := filepath.Join(home, coagenthome.DirName, coagenthome.DBFileName)
	seedDB, err := OpenDB(ctx, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = seedDB.Close() })
	_, err = seedDB.ExecContext(ctx, "PRAGMA wal_autocheckpoint=0")
	require.NoError(t, err)
	provider := newProvider(t, seedDB)
	_, err = provider.UpTo(ctx, 21)
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, `
		INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/wal', 'wal')`)
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, `INSERT INTO sessions (id, project_id) VALUES (1, 1)`)
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, `
		INSERT INTO session_inbox (session_id, source, raw_content, received_at)
		VALUES (1, 'user', 'from wal', '2026-08-18 00:00:00')`)
	require.NoError(t, err)

	db, err := Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	backupPath := dbPath + ".bak"
	backupInfo, err := os.Stat(backupPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), backupInfo.Mode().Perm())
	backupDB, err := sql.Open("sqlite", backupPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = backupDB.Close() })

	assert.Equal(t, int64(21), currentVersion(t, backupDB))
	var content string
	require.NoError(t, backupDB.QueryRowContext(ctx,
		`SELECT raw_content FROM session_inbox WHERE session_id = 1`).Scan(&content))
	assert.Equal(t, "from wal", content)
	assert.Equal(t, int64(23), currentVersion(t, db))
}

func TestOpenRefusesMigrationWhenBackupPublicationCannotProceed(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	t.Cleanup(restore)

	ctx := t.Context()
	dbPath := filepath.Join(home, coagenthome.DirName, coagenthome.DBFileName)
	seedDB, err := OpenDB(ctx, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = seedDB.Close() })
	provider := newProvider(t, seedDB)
	_, err = provider.UpTo(ctx, 21)
	require.NoError(t, err)
	_, err = seedDB.ExecContext(ctx, `
		INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/preserved', 'preserved')`)
	require.NoError(t, err)

	backupPath := dbPath + ".bak"
	require.NoError(t, os.Mkdir(backupPath, 0o755))

	_, err = Open(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup database")
	assert.Equal(t, int64(21), currentVersion(t, seedDB))
	var name string
	require.NoError(t, seedDB.QueryRowContext(ctx,
		`SELECT name FROM projects WHERE id = 1`).Scan(&name))
	assert.Equal(t, "preserved", name)

	entries, err := os.ReadDir(filepath.Dir(backupPath))
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), ".daemon.db.bak.stage-"),
			"temporary backup must be removed: %s", entry.Name())
	}
	_, err = os.Stat(backupPath)
	require.NoError(t, err)
}

func currentVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var version int64
	err := db.QueryRowContext(t.Context(), `
		SELECT version_id FROM goose_db_version
		WHERE is_applied = 1 ORDER BY id DESC LIMIT 1`).Scan(&version)
	require.NoError(t, err)

	return version
}

func TestBackupDBRejectsUnreadableDestination(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "daemon.db")
	db, err := OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	backupPath := dbPath + ".bak"
	require.NoError(t, os.Mkdir(backupPath, 0o755))
	err = backupDB(t.Context(), db, dbPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup destination is not a regular file")
}

func TestPreserveBackupKeepsCanonicalPathUntilPublication(t *testing.T) {
	t.Parallel()

	bakPath := filepath.Join(t.TempDir(), "daemon.db.bak")
	require.NoError(t, os.WriteFile(bakPath, []byte("previous"), 0o600))

	oldPath, err := preserveBackup(bakPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(oldPath) })

	canonical, err := os.ReadFile(bakPath)
	require.NoError(t, err)
	preserved, err := os.ReadFile(oldPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("previous"), canonical)
	assert.Equal(t, canonical, preserved)
}
