package managercontrol

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

type appTestControl interface {
	controllerapi.Controller
	controllerapi.ChatFocusController
	controllerapi.LineageController
}

type appBackendCall struct {
	verb, app, text, key string
	id                   int64
}

type appTestBackend struct {
	Backend
	calls []appBackendCall
}

type appFixture struct {
	factory controllerapi.ManagerControllerFactory
	store   *sessionstore.Store
	db      *sql.DB
	backend *appTestBackend
	p, q    string
}

func (b *appTestBackend) SpawnForApp(_ context.Context, app, dir, worktree, model string) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "spawn", app: app, text: dir, key: worktree + model})
	return 77, nil
}

func (b *appTestBackend) SendForApp(_ context.Context, app string, id int64, text, key string) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "send", app: app, id: id, text: text, key: key})
	return id + 1, nil
}

func (b *appTestBackend) StopForApp(_ context.Context, app string, id int64) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "stop", app: app, id: id})
	return id + 1, nil
}

func (b *appTestBackend) KillForApp(_ context.Context, app string, id int64) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "kill", app: app, id: id})
	return id + 1, nil
}

func (b *appTestBackend) TurnOver(_ context.Context, id int64) (int64, bool, error) {
	b.calls = append(b.calls, appBackendCall{verb: "outcome", id: id})
	return id + 1, true, nil
}

func (b *appTestBackend) SendToSessionResolved(_ context.Context, id int64, text, key string) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "human", id: id, text: text, key: key})
	return id + 1, nil
}

func (b *appTestBackend) CreateSession(context.Context, int64, string, string, string, map[string]any) (int64, error) {
	b.calls = append(b.calls, appBackendCall{verb: "create"})
	return 77, nil
}

func (b *appTestBackend) SetModel(_ context.Context, id int64, model, effort string) error {
	b.calls = append(b.calls, appBackendCall{verb: "model", id: id, text: model, key: effort})
	return nil
}

func (*appTestBackend) HasActiveLoop(int64) bool { return false }

func (*appTestBackend) NotifySession(int64, sessionevent.Notification) {}

func newAppFixture(t *testing.T) *appFixture {
	t.Helper()
	_, store, _, db := newChatControl(t)
	projects := t.TempDir()
	p, q := filepath.Join(projects, "p"), filepath.Join(projects, "q")
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		ProjectsRoot: projects, SpawnFavorites: []string{p, q},
	}}
	backend := &appTestBackend{}
	factory := New(backend, store, managerdiscovery.New(store, cfg, nil), sessionbus.New(), nil)
	return &appFixture{factory: factory, store: store, db: db, backend: backend, p: p, q: q}
}

func (f *appFixture) control(t *testing.T, projects []string) appTestControl {
	t.Helper()
	control, ok := f.factory.ForApp("bot", projects).(appTestControl)
	require.True(t, ok)
	return control
}

func (f *appFixture) root(t *testing.T, dir string, attrs map[string]any) int64 {
	t.Helper()
	project, err := f.store.GetOrCreateProject(t.Context(), dir)
	require.NoError(t, err)
	return sessionstoretest.BareRoot(t, f.db, project, "model", "", attrs)
}
