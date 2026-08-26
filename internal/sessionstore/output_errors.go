package sessionstore

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The store owns SQLite, so replay detection keys on the driver's typed
// constraint error rather than a driver-formatted message string.
func isUniqueConstraintError(err error) bool {
	var sqliteErr *sqlite.Error

	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}
