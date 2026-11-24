package managercontrol

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

type failingSessionLookup struct {
	Store
	err error
}

func (s failingSessionLookup) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	return nil, s.err
}

func TestRootCheckPreservesSessionLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "storage error", err: errors.New("storage unavailable")},
		{name: "missing row", err: sql.ErrNoRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &service{store: failingSessionLookup{err: tc.err}}
			_, err := s.requireRoot(t.Context(), 42)
			if errors.Is(tc.err, sql.ErrNoRows) {
				require.EqualError(t, err, "session 42 not found")
				return
			}
			require.ErrorIs(t, err, tc.err)
			require.ErrorContains(t, err, "session 42")
		})
	}
}
