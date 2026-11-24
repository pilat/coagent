package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

type failingAppProjectLookup struct {
	Store
	err error
}

func (s failingAppProjectLookup) GetProjectWorkDir(context.Context, int64) (string, error) {
	return "", s.err
}

func TestAppScopePreservesStorageErrors(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	id := f.root(t, f.p, nil)
	failure := errors.New("storage unavailable")
	for _, test := range []struct {
		name  string
		store Store
		want  string
	}{
		{name: "session", store: failingSessionLookup{err: failure}, want: fmt.Sprintf("load session %d", id)},
		{
			name: "project", store: failingAppProjectLookup{Store: f.store, err: failure},
			want: fmt.Sprintf("read session %d project", id),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			factory := New(f.backend, test.store, nil, nil, nil)
			app := factory.ForApp("bot", []string{f.p}).(controllerapi.ChatFocusController)
			_, err := app.SessionKilled(t.Context(), id)
			require.ErrorIs(t, err, failure)
			require.ErrorContains(t, err, test.want)
		})
	}
}
