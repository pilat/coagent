package migrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A raw '#' or '?' in the path is a URI fragment/query delimiter: SQLite would
// silently open a truncated sibling path instead (fuzz subtests named "seed#0"
// hit exactly this through t.TempDir).
func TestOpenDB_DelimiterCharactersInPathOpenTheExactFile(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{"seed#0", "q?mark", "pct%41"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(base, dir), 0o755))
			dbPath := filepath.Join(base, dir, "exact.db")

			db, err := OpenDB(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			_, err = db.ExecContext(t.Context(), `CREATE TABLE probe (id INTEGER)`)
			require.NoError(t, err)

			_, err = os.Stat(dbPath)
			require.NoError(t, err, "database must land at the exact requested path")

			var foreignKeys int
			require.NoError(t, db.QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&foreignKeys))
			require.Equal(t, 1, foreignKeys, "DSN params must survive delimiter characters in the path")

			entries, err := os.ReadDir(base)
			require.NoError(t, err)
			require.Len(t, entries, 1, "no truncated sibling file next to the intended directory")
		})
	}
}
