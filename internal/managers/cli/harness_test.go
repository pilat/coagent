package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
)

const chatProjectID = 7

var _ controllerapi.ChatController = (*fakeController)(nil)

// fakeController is the Controller surface the chat manager actually uses.
type fakeController struct {
	mu sync.Mutex

	sessions []controllerapi.SessionInfo
	created  []controllerapi.SessionCreateData
	projects []controllerapi.ProjectCreateData
	sent     []controllerapi.SessionMessageData
	models   []controllerapi.ConfigModelInfo
	setModel []controllerapi.SessionSetModelData
	setAttrs []controllerapi.SessionSetAttributesData
	nextID   int64

	events chan controllerapi.SessionNotification
}

func newFakeController() *fakeController {
	return &fakeController{nextID: 100, events: make(chan controllerapi.SessionNotification, 16)}
}

func (f *fakeController) CreateProject(
	_ context.Context, data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	f.projects = append(f.projects, data)

	return &controllerapi.ProjectCreateResultData{
		ID: chatProjectID, Name: ProjectName, Path: "/projects/sys_coagent",
	}, nil
}

func (f *fakeController) CreateSession(_ context.Context, d controllerapi.SessionCreateData) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	f.created = append(f.created, d)
	attributes := maps.Clone(d.Attributes)
	if attributes == nil {
		attributes = make(map[string]any)
	}
	attributes[controllerapi.SessionAttributeManagerID] = controllerapi.BuiltinCLIManagerID
	f.sessions = append(f.sessions, controllerapi.SessionInfo{
		ID: f.nextID, ProjectID: chatProjectID, UpdatedAt: time.Now(), Attributes: attributes,
	})

	return f.nextID, nil
}

func (f *fakeController) SendSessionMessage(_ context.Context, d controllerapi.SessionMessageData) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sent = append(f.sent, d)

	return nil
}

func (f *fakeController) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return &controllerapi.ConfigModelsResultData{
		Models: append([]controllerapi.ConfigModelInfo(nil), f.models...),
	}, nil
}

func (f *fakeController) SetSessionModel(_ context.Context, d controllerapi.SessionSetModelData) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.setModel = append(f.setModel, d)

	return nil
}

func (f *fakeController) SetSessionAttributes(
	_ context.Context,
	d controllerapi.SessionSetAttributesData,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.setAttrs = append(f.setAttrs, d)
	for i := range f.sessions {
		if f.sessions[i].ID == d.SessionID {
			f.sessions[i].Attributes = d.Attributes
		}
	}

	return nil
}

func (f *fakeController) ListSessions(context.Context) ([]controllerapi.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	sessions := append([]controllerapi.SessionInfo(nil), f.sessions...)
	for i := range sessions {
		if sessions[i].ProjectID != chatProjectID || sessions[i].Attributes != nil {
			continue
		}

		sessions[i].Attributes = map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}
	}

	return sessions, nil
}

func (f *fakeController) Subscribe() <-chan controllerapi.SessionNotification { return f.events }

func (f *fakeController) Unsubscribe(<-chan controllerapi.SessionNotification) {}

type harness struct {
	ctrl    *fakeController
	secrets *fakeSecrets
	mgr     *Manager
	socket  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	// A unix socket path is capped near 100 bytes; a deep TMPDIR blows it.
	dir, err := os.MkdirTemp("/tmp", "cli")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "d.sock")

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{})
	require.NoError(t, err)

	t.Cleanup(func() { _ = srv.Close() })

	ctrl := newFakeController()
	secrets := newFakeSecrets()
	mgr := New(ctrl, srv, "claude-sonnet-5", secrets)
	require.NoError(t, mgr.Start(context.Background()))

	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })

	go func() { _ = srv.Serve(context.Background()) }()

	return &harness{ctrl: ctrl, secrets: secrets, mgr: mgr, socket: socket}
}

func (h *harness) dial(t *testing.T) *ctl.Client {
	t.Helper()

	c, err := ctl.Dial(context.Background(), h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func openChat(t *testing.T, c *ctl.Client) OpenResult {
	t.Helper()

	var res OpenResult
	require.NoError(t, c.Call(context.Background(), OpChatOpen, struct{}{}, &res))

	return res
}

func sendChat(t *testing.T, c *ctl.Client, p SendParams) SendResult {
	t.Helper()

	var res SendResult
	require.NoError(t, c.Call(context.Background(), OpChatSend, p, &res))

	return res
}
