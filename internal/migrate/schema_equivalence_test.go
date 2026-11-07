package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaEquivalenceLive(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "live.db")
	db, err := OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ddl, err := os.ReadFile("testdata/schema_live_v46.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(ddl))
	require.NoError(t, err)
	for version := range 47 {
		_, err = db.ExecContext(t.Context(),
			`INSERT INTO goose_db_version(version_id, is_applied) VALUES (?, 1)`, version)
		require.NoError(t, err)
	}

	provider, err := NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	assertSchemaGolden(t, db, "schema_live.golden")
}

func TestSchemaEquivalenceFresh(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = newProvider(t, db).UpTo(t.Context(), 46)
	require.NoError(t, err)

	provider, err := NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	assertSchemaGolden(t, db, "schema_fresh.golden")
}

func assertSchemaGolden(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	assert.Equal(t, string(want), normalizedSchema(t, db))
	assert.Equal(t, []string{"47", "48", "49", "50"}, stringColumn(t, db,
		`SELECT CAST(version_id AS TEXT) FROM goose_db_version
		WHERE version_id > 46 AND is_applied = 1 ORDER BY id`))
}

func normalizedSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version' ORDER BY type, name`)
	require.NoError(t, err)
	defer rows.Close()
	var schema strings.Builder
	for rows.Next() {
		var kind, name, table, ddl string
		require.NoError(t, rows.Scan(&kind, &name, &table, &ddl))
		schema.WriteString(strings.Join([]string{kind, name, table, strings.Join(strings.Fields(ddl), " ")}, "\t"))
		schema.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return schema.String()
}
