package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

const testVersion = "0.4.2"

var testConfigPath = "~/" + coagenthome.DirName + "/" + coagenthome.ConfigFileName

// fakeManagers reports a fixed running set and per-manager start errors, so
// status can be observed without a real manager runtime.
type fakeManagers struct {
	mu      sync.Mutex
	running []string
	errs    map[string]error
}

func (f *fakeManagers) RunningIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.running
}

func (f *fakeManagers) StartError(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.errs[id]
}

func (f *fakeManagers) fail(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.errs == nil {
		f.errs = map[string]error{}
	}

	f.errs[id] = err
}

type harness struct {
	socket   string
	server   *Server
	managers *fakeManagers
}

func newHarness(t *testing.T, cfg *config.Config) *harness {
	return newHarnessWithRegistration(t, cfg, nil)
}

func newHarnessWithRegistration(t *testing.T, cfg *config.Config, register func(*Server)) *harness {
	t.Helper()

	mgrs := &fakeManagers{}
	h := &harness{socket: socketPath(t), managers: mgrs}

	srv, err := NewServer(context.Background(), h.socket, testVersion, Deps{
		Config:     cfg,
		ConfigPath: testConfigPath,
		Managers:   mgrs,
	})
	require.NoError(t, err)

	h.server = srv
	if register != nil {
		register(srv)
	}

	go func() { _ = srv.Serve(context.Background()) }()
	<-srv.serveReady

	t.Cleanup(func() { _ = srv.Close() })

	return h
}

func (h *harness) dial(t *testing.T) *Client {
	t.Helper()

	c, err := Dial(context.Background(), h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

// socketPath keeps the unix path under the 100-byte sun_path limit even when
// TMPDIR is deep, which a plain t.TempDir() join does not guarantee.
func socketPath(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if p := filepath.Join(dir, "d.sock"); len(p) <= maxSocketPath {
		return p
	}

	short, err := os.MkdirTemp("/tmp", "ctl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	return filepath.Join(short, "d.sock")
}

func enabled() *bool { v := true; return &v }

func TestServer_GreetingAndSocketMode(t *testing.T) {
	h := newHarness(t, &config.Config{})
	c := h.dial(t)

	g := c.Greeting()
	assert.Equal(t, AppName, g.App)
	assert.Equal(t, testVersion, g.BinaryVersion)
	assert.Equal(t, ProtocolVersion, g.ProtocolVersion)
	assert.False(t, c.SkewsFrom())

	info, err := os.Stat(h.socket)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStatus_WithoutConfig(t *testing.T) {
	h := newHarness(t, &config.Config{})
	c := h.dial(t)

	st, err := c.Status(context.Background())
	require.NoError(t, err)

	assert.False(t, st.ConfigPresent)
	assert.Equal(t, testConfigPath, st.ConfigPath)
	assert.Equal(t, testVersion, st.BinaryVersion)
	assert.Equal(t, ProtocolVersion, st.ProtocolVersion)
	assert.Equal(t, os.Getpid(), st.PID)
	assert.Empty(t, st.Providers)
	assert.Empty(t, st.Managers)
	assert.Equal(t, 0, st.ModelCount)
}

// A restart reuses the pid, socket and binary, so the boot id is the only thing a
// client can compare to tell "it came back" from "the old run still answers".
func TestStatus_BootIDNamesTheRunNotTheBinary(t *testing.T) {
	first := newHarness(t, &config.Config{}).dial(t)

	st, err := first.Status(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, st.BootID)

	again, err := first.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, st.BootID, again.BootID, "one run, one id")

	second := newHarness(t, &config.Config{}).dial(t)

	other, err := second.Status(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, st.BootID, other.BootID)
	assert.Equal(t, st.BinaryVersion, other.BinaryVersion, "same binary — the version cannot tell them apart")
}

func TestStatus_WithConfig(t *testing.T) {
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"work":     {Driver: "anthropic"},
			"fallback": {Driver: "openrouter"},
		},
		Models: []config.ModelEntry{
			{ID: "claude-sonnet-5", Provider: "work"},
			{ID: "gpt-5", Provider: "fallback"},
		},
		Managers: []config.ManagerEntry{
			{ID: "tg", Driver: "telegram", Enabled: enabled()},
			{ID: "tg-down", Driver: "telegram", Enabled: enabled()},
		},
	}}

	h := newHarness(t, cfg)
	h.managers.running = []string{"tg"}
	h.managers.fail("tg-down", errors.New("ensure service topic: unauthorized"))

	c := h.dial(t)

	st, err := c.Status(context.Background())
	require.NoError(t, err)

	assert.True(t, st.ConfigPresent)
	assert.Equal(t, 2, st.ModelCount)
	assert.Equal(t, "claude-sonnet-5", st.DefaultModel)

	// Providers come back sorted so a UI renders them stably.
	assert.Equal(t, []ProviderStatus{
		{Name: "fallback", Driver: "openrouter"},
		{Name: "work", Driver: "anthropic"},
	}, st.Providers)

	require.Len(t, st.Managers, 2)
	assert.Equal(t, ManagerStatus{ID: "tg", Driver: "telegram", Enabled: true, Running: true}, st.Managers[0])
	assert.Equal(t, "tg-down", st.Managers[1].ID)
	assert.False(t, st.Managers[1].Running)
	assert.Contains(t, st.Managers[1].Error, "unauthorized")
}

func TestStatus_DisabledManagerCarriesNoError(t *testing.T) {
	off := false
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Managers: []config.ManagerEntry{{ID: "tg", Driver: "telegram", Enabled: &off}},
	}}

	h := newHarness(t, cfg)
	h.managers.fail("tg", errors.New("boom"))

	st, err := h.dial(t).Status(context.Background())
	require.NoError(t, err)

	require.Len(t, st.Managers, 1)
	assert.False(t, st.Managers[0].Enabled)
	assert.Empty(t, st.Managers[0].Error)
}

// `status` is the only diagnostic a chat has for "your bot token is wrong", so a
// manager that is down for its own reason must not be handed another manager's.
func TestStatus_ADownManagerCarriesItsOwnReasonOnly(t *testing.T) {
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Managers: []config.ManagerEntry{
			{ID: "tg-one", Driver: "telegram", Enabled: enabled()},
			{ID: "tg-two", Driver: "telegram", Enabled: enabled()},
		},
	}}

	h := newHarness(t, cfg)
	h.managers.fail("tg-one", errors.New(`start manager "tg-one": unauthorized`))

	st, err := h.dial(t).Status(context.Background())
	require.NoError(t, err)

	require.Len(t, st.Managers, 2)
	assert.Contains(t, st.Managers[0].Error, "unauthorized")
	assert.Empty(t, st.Managers[1].Error, "tg-two is down for its own reason, not tg-one's")
}

// A push that lands between a request and its response must not be read as the
// response. This is the whole reason the client is a demultiplexer.
func TestClient_NotificationBetweenRequestAndResponse(t *testing.T) {
	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("slow_echo", func(
			_ context.Context,
			conn *Conn,
			params json.RawMessage,
		) (any, *Error) {
			for i := range 3 {
				require.NoError(t, conn.Notify("chat_event", map[string]int{"seq": i}))
			}

			return map[string]json.RawMessage{"echo": params}, nil
		}))
	})

	c := h.dial(t)

	var out map[string]any

	require.NoError(t, c.Call(context.Background(), "slow_echo", map[string]string{"say": "hi"}, &out))
	assert.Equal(t, map[string]any{"say": "hi"}, out["echo"])

	for i := range 3 {
		select {
		case n := <-c.Notifications():
			assert.Equal(t, "chat_event", n.Method)
			assert.JSONEq(t, `{"seq":`+string(rune('0'+i))+`}`, string(n.Params))
		case <-time.After(2 * time.Second):
			t.Fatalf("notification %d never arrived", i)
		}
	}
}

func TestClient_UnknownMethodIsAnRPCError(t *testing.T) {
	c := newHarness(t, &config.Config{}).dial(t)

	err := c.Call(context.Background(), "no_such_op", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown method no_such_op")
}

func TestClient_HandlerErrorIsAnRPCError(t *testing.T) {
	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("boom", func(context.Context, *Conn, json.RawMessage) (any, *Error) {
			return nil, &Error{Code: CodeInvalidParams, Message: "name is required"}
		}))
	})

	err := h.dial(t).Call(context.Background(), "boom", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestServer_RejectsHandlerRegistrationAfterServing(t *testing.T) {
	h := newHarness(t, &config.Config{})

	err := h.server.Register("late", func(context.Context, *Conn, json.RawMessage) (any, *Error) {
		return nil, nil
	})
	require.ErrorIs(t, err, ErrRegistrationClosed)
}

func TestServer_RejectsInvalidHandlerRegistrations(t *testing.T) {
	server, err := NewServer(context.Background(), socketPath(t), "test", Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	handler := func(context.Context, *Conn, json.RawMessage) (any, *Error) { return "first", nil }
	require.NoError(t, server.Register("custom", handler))

	tests := []struct {
		name    string
		op      string
		handler Handler
		wantErr error
	}{
		{name: "empty operation", handler: handler, wantErr: ErrInvalidRegistration},
		{name: "blank operation", op: "  ", handler: handler, wantErr: ErrInvalidRegistration},
		{name: "nil handler", op: "nil_handler", wantErr: ErrInvalidRegistration},
		{name: "built-in status", op: OpStatus, handler: handler, wantErr: ErrOperationReserved},
		{name: "duplicate", op: "custom", handler: handler, wantErr: ErrHandlerRegistered},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := server.Register(tt.op, tt.handler)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}

	registered, ok := server.handler("custom")
	require.True(t, ok)
	got, rpcErr := registered(context.Background(), nil, nil)
	require.Nil(t, rpcErr)
	assert.Equal(t, "first", got, "a rejected duplicate must not replace the original handler")
}

func TestServer_RejectsHandlerRegistrationAfterClose(t *testing.T) {
	server, err := NewServer(context.Background(), socketPath(t), "test", Deps{})
	require.NoError(t, err)
	require.NoError(t, server.Close())

	err = server.Register("late", func(context.Context, *Conn, json.RawMessage) (any, *Error) {
		return nil, nil
	})
	require.ErrorIs(t, err, ErrRegistrationClosed)
}

func TestServer_ConcurrentDuplicateRegistrationKeepsExactlyOneHandler(t *testing.T) {
	server, err := NewServer(context.Background(), socketPath(t), "test", Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	const candidates = 16
	errs := make(chan error, candidates)
	for candidate := range candidates {
		go func() {
			err := server.Register("contended", func(context.Context, *Conn, json.RawMessage) (any, *Error) {
				return candidate, nil
			})
			errs <- err
		}()
	}

	var accepted int
	for range candidates {
		err := <-errs
		if err == nil {
			accepted++
			continue
		}

		require.ErrorIs(t, err, ErrHandlerRegistered)
	}
	assert.Equal(t, 1, accepted)

	registered, ok := server.handler("contended")
	require.True(t, ok)
	got, rpcErr := registered(context.Background(), nil, nil)
	require.Nil(t, rpcErr)
	assert.IsType(t, int(0), got)
}

func TestDial_NotRunning(t *testing.T) {
	_, err := Dial(context.Background(), filepath.Join(t.TempDir(), "absent.sock"))
	require.ErrorIs(t, err, ErrNotRunning)
}

func TestClient_CallAfterServerCloseFails(t *testing.T) {
	h := newHarness(t, &config.Config{})
	c := h.dial(t)

	require.NoError(t, h.server.Close())

	// The push stream closing is how a chat client learns the daemon went away.
	select {
	case _, ok := <-c.Notifications():
		assert.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("notification stream never closed")
	}

	err := c.Call(context.Background(), OpStatus, nil, nil)
	require.Error(t, err)
}

func TestServer_CloseRemovesSocket(t *testing.T) {
	h := newHarness(t, &config.Config{})
	require.NoError(t, h.server.Close())

	_, err := os.Stat(h.socket)
	assert.True(t, os.IsNotExist(err))
}

func TestLock_SecondDaemonFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")

	first, err := Acquire(path)
	require.NoError(t, err)

	_, err = Acquire(path)
	require.ErrorIs(t, err, ErrAlreadyRunning)

	require.NoError(t, first.Release())

	second, err := Acquire(path)
	require.NoError(t, err)
	require.NoError(t, second.Release())
}
