package localapi

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ managercontrol.Backend = (*apiBackend)(nil)

type apiSend struct {
	sessionID int64
	text      string
	key       string
}

type apiBackend struct {
	store     *sessionstore.Store
	bus       *sessionbus.Bus
	mu        sync.Mutex
	creates   int
	create    func(context.Context)
	worktrees []string
	models    []controllerapi.SessionSetModelData
	sends     []apiSend
	send      func(context.Context) error
}

type apiFixture struct {
	store   *sessionstore.Store
	bus     *sessionbus.Bus
	backend *apiBackend
	factory controllerapi.ManagerControllerFactory
	entries []Client
	tokens  map[string]string
}

type apiServer struct {
	srv          *Server
	socket       string
	statusSocket string
	done         chan error
	closed       bool
	serving      int
	addr         string
}

func (b *apiBackend) CreateSession(
	ctx context.Context,
	project int64,
	worktree, _ string,
	model string,
	attrs map[string]any,
) (int64, error) {
	b.mu.Lock()
	b.creates++
	b.worktrees = append(b.worktrees, worktree)
	create := b.create
	b.mu.Unlock()

	if create != nil {
		create(ctx)
	}

	workDir, err := b.store.GetProjectWorkDir(ctx, project)
	if err != nil {
		return 0, fmt.Errorf("load test project: %w", err)
	}
	root, _, err := b.store.CreateRoot(ctx, sessionstore.RootCreate{
		ProjectID: project, Model: model, Attributes: attrs, Name: "test", WorkDir: workDir,
	})
	if err != nil {
		return 0, fmt.Errorf("create test root: %w", err)
	}
	return root.ID, nil
}

func (b *apiBackend) SendToSessionResolved(ctx context.Context, id int64, text, key string) (int64, error) {
	b.mu.Lock()
	b.sends = append(b.sends, apiSend{sessionID: id, text: text, key: key})
	send := b.send
	b.mu.Unlock()

	if send == nil {
		return id, nil
	}

	if err := send(ctx); err != nil {
		return 0, fmt.Errorf("blocked test send: %w", err)
	}

	return id, nil
}

func (b *apiBackend) SetModel(_ context.Context, id int64, model, effort string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.models = append(b.models, controllerapi.SessionSetModelData{SessionID: id, Model: model, ReasoningLevel: effort})
	return nil
}

func (b *apiBackend) HasActiveLoop(int64) bool { return false }

func (b *apiBackend) NotifySession(id int64, n sessionevent.Notification) {
	record, err := b.store.GetSession(context.Background(), id)
	if err == nil {
		b.bus.Publish(id, record.ParentID == 0, n)
	}
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	db, err := migrate.OpenDB(t.Context(), filepath.Join(t.TempDir(), "api.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	bus := sessionbus.New()
	backend := &apiBackend{store: store, bus: bus}
	f := &apiFixture{
		store: store, bus: bus, backend: backend,
		factory: managercontrol.New(backend, store, nil, bus, nil),
		tokens:  make(map[string]string),
	}
	for _, id := range []string{"alpha", "beta"} {
		token, err := NewToken()
		require.NoError(t, err)
		f.tokens[id] = token
		f.entries = append(f.entries, Client{
			ID: id, TokenSHA256: TokenHash(token),
		})
	}
	return f
}

func apiListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "api")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	path := filepath.Join(dir, "api.sock")
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o600))
	return listener, path
}

func (f *apiFixture) clients() []Client {
	clients := append([]Client(nil), f.entries...)
	for i := range clients {
		if clients[i].Control == nil {
			clients[i].Control = f.factory.ForManager(clients[i].ID)
		}
	}
	return clients
}

func (f *apiFixture) start(t *testing.T) *apiServer {
	t.Helper()
	listener, socket := apiListener(t)
	return f.serve(t, listener, socket)
}

func (h *apiServer) close(t *testing.T) {
	t.Helper()
	if h.closed {
		return
	}
	h.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, h.srv.Close(ctx))
	for range h.serving {
		select {
		case err := <-h.done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("API Serve did not finish")
		}
	}
}

func (f *apiFixture) root(t *testing.T, h *apiServer, owner string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens[owner]))
	response, err := client.CreateSession(ctx, &coagentv1.CreateSessionRequest{WorkDir: t.TempDir(), Model: "model"})
	require.NoError(t, err)
	return response.GetSessionId()
}

func (f *apiFixture) append(t *testing.T, id int64, text string) int64 {
	t.Helper()
	commit, err := f.store.EnqueueOutput(t.Context(), sessionstore.OutputDraft{
		SessionID: id, Type: sessionstore.OutputMessage, Content: text,
	})
	require.NoError(t, err)
	return commit.OutputID
}

func (f *apiFixture) wake(id int64) {
	f.bus.Publish(id, true, sessionevent.Notification{Type: sessionevent.NotifyHeartbeat})
}

func (f *apiFixture) tail(
	t *testing.T,
	h *apiServer,
	id, after int64,
) coagentv1connect.OutputServiceTailOutputClientStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	t.Cleanup(cancel)
	client := coagentv1connect.NewOutputServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	stream, err := client.TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: id, AfterId: after})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

func nextOutput(t *testing.T, stream coagentv1connect.OutputServiceTailOutputClientStream) *coagentv1.OutputRow {
	t.Helper()
	for {
		event, err := stream.Receive()
		require.NoError(t, err)
		if output := event.GetOutput(); output != nil {
			return output
		}
	}
}
