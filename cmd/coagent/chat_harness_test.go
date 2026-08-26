package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/managers/cli"
)

// maxSocketPath is the sun_path limit, checked here because a deep TMPDIR makes
// t.TempDir() unusable for a unix socket.
const maxSocketPath = 100

// chatServer is a real control server with scripted chat ops, so the client, the
// framing and the push stream under test are all production code.
type chatServer struct {
	socket    string
	server    *ctl.Server
	sessionID int64

	mu       sync.Mutex
	conns    []*ctl.Conn
	sent     []cli.SendParams
	stops    []int64
	models   []controllerapi.ConfigModelInfo
	current  string
	effort   string
	setModel []cli.SetModelParams
	secrets  []ctl.SetSecretParams
	declined []cli.SecretCancelParams
}

func newChatServer(t *testing.T, socket string, sessionID int64) *chatServer {
	t.Helper()

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{Config: &config.Config{}})
	require.NoError(t, err)

	s := &chatServer{socket: socket, server: srv, sessionID: sessionID}

	require.NoError(t, srv.Register(cli.OpChatOpen, s.openOp))
	require.NoError(t, srv.Register(cli.OpChatSend, s.sendOp))
	require.NoError(t, srv.Register(cli.OpChatStop, s.stopOp))
	require.NoError(t, srv.Register(cli.OpChatModels, s.modelsOp))
	require.NoError(t, srv.Register(cli.OpChatSetModel, s.setModelOp))
	require.NoError(t, srv.Register(ctl.OpSetSecret, s.secretOp))
	require.NoError(t, srv.Register(cli.OpChatSecretCancel, s.cancelOp))

	go func() { _ = srv.Serve(context.Background()) }()

	t.Cleanup(func() { _ = srv.Close() })

	waitServing(t, socket)

	return s
}

// push sends one notification to every attached terminal.
func (s *chatServer) push(t *testing.T, method string, params any) {
	t.Helper()

	require.Eventually(t, func() bool { return len(s.attached()) > 0 }, 5*time.Second, 5*time.Millisecond)

	for _, c := range s.attached() {
		require.NoError(t, c.Notify(method, params))
	}
}

func (s *chatServer) attached() []*ctl.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]*ctl.Conn(nil), s.conns...)
}

func (s *chatServer) sentText() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.sent))
	for _, p := range s.sent {
		out = append(out, p.Text)
	}

	return out
}

func (s *chatServer) sentParams() []cli.SendParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]cli.SendParams(nil), s.sent...)
}

func (s *chatServer) storedSecrets() []ctl.SetSecretParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]ctl.SetSecretParams(nil), s.secrets...)
}

func (s *chatServer) stopped() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]int64(nil), s.stops...)
}

func (s *chatServer) modelChanges() []cli.SetModelParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]cli.SetModelParams(nil), s.setModel...)
}

func (s *chatServer) declinedSecrets() []cli.SecretCancelParams {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]cli.SecretCancelParams(nil), s.declined...)
}

func (s *chatServer) cancelOp(_ context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
	var p cli.SecretCancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.declined = append(s.declined, p)
	s.mu.Unlock()

	return cli.SendResult{SessionID: p.SessionID}, nil
}

func (s *chatServer) openOp(_ context.Context, c *ctl.Conn, _ json.RawMessage) (any, *ctl.Error) {
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()

	return cli.OpenResult{SessionID: s.sessionID}, nil
}

func (s *chatServer) sendOp(_ context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
	var p cli.SendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.sent = append(s.sent, p)
	s.mu.Unlock()

	return cli.SendResult{SessionID: s.sessionID}, nil
}

func (s *chatServer) stopOp(_ context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
	var p cli.SessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.stops = append(s.stops, p.SessionID)
	s.mu.Unlock()

	return cli.SendResult{SessionID: p.SessionID}, nil
}

func (s *chatServer) modelsOp(_ context.Context, _ *ctl.Conn, _ json.RawMessage) (any, *ctl.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cli.ModelsResult{
		Models:    append([]controllerapi.ConfigModelInfo(nil), s.models...),
		CurrentID: s.current, CurrentEffort: s.effort,
	}, nil
}

func (s *chatServer) setModelOp(_ context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
	var p cli.SetModelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.setModel = append(s.setModel, p)
	s.mu.Unlock()

	return cli.SendResult{SessionID: p.SessionID}, nil
}

func (s *chatServer) secretOp(_ context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
	var p ctl.SetSecretParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
	}

	s.mu.Lock()
	s.secrets = append(s.secrets, p)
	s.mu.Unlock()

	return struct{}{}, nil
}

// scriptedTerminal is the input device under test control. With idle unset a
// line read blocks, which is exactly the state the old code raced against.
type scriptedTerminal struct {
	lines   chan string
	secrets chan string
	idle    time.Duration

	entered chan struct{}

	// masked counts prompt openings, not reads: a polled masked read re-enters
	// the mode it is already in every poll window.
	masked atomic.Int32
	inMask atomic.Bool
}

func newScriptedTerminal(idle time.Duration) *scriptedTerminal {
	return &scriptedTerminal{
		lines:   make(chan string),
		secrets: make(chan string),
		idle:    idle,
		entered: make(chan struct{}, 64),
	}
}

func (s *scriptedTerminal) ReadLine() (string, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}

	if s.idle == 0 {
		return recvLine(s.lines, nil)
	}

	idle := time.NewTimer(s.idle)
	defer idle.Stop()

	return recvLine(s.lines, idle.C)
}

// ReadSecret mirrors the real device: echo is off for the whole prompt, and the
// read is polled so the loop keeps regaining control between keystrokes.
func (s *scriptedTerminal) ReadSecret() (string, error) {
	if s.inMask.CompareAndSwap(false, true) {
		s.masked.Add(1)
	}

	if s.idle == 0 {
		value, err := recvLine(s.secrets, nil)
		s.inMask.Store(false)

		return value, err
	}

	idle := time.NewTimer(s.idle)
	defer idle.Stop()

	value, err := recvLine(s.secrets, idle.C)
	if errors.Is(err, errNoInput) {
		return "", err
	}

	s.inMask.Store(false)

	return value, err
}

func (s *scriptedTerminal) EndSecret() { s.inMask.Store(false) }

// syncBuffer collects rendered output from both the input loop and the push
// reader, which write concurrently by design.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func recvLine(lines <-chan string, idle <-chan time.Time) (string, error) {
	select {
	case line, ok := <-lines:
		if !ok {
			return "", io.EOF
		}

		return line, nil
	case <-idle:
		return "", errNoInput
	}
}

// socketPath keeps the unix path under the sun_path limit even when TMPDIR is
// deep, which a plain t.TempDir() join does not guarantee.
func socketPath(t *testing.T) string {
	t.Helper()

	if p := filepath.Join(t.TempDir(), "d.sock"); len(p) <= maxSocketPath {
		return p
	}

	short, err := os.MkdirTemp("/tmp", "coagentchat")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	return filepath.Join(short, "d.sock")
}

// waitServing blocks until the socket answers with a greeting: the listener is
// bound before Serve runs, so connecting alone proves nothing.
func waitServing(t *testing.T, socket string) {
	t.Helper()

	require.Eventually(t, func() bool {
		client, err := ctl.Dial(context.Background(), socket)
		if err != nil {
			return false
		}

		_ = client.Close()

		return true
	}, 10*time.Second, 10*time.Millisecond)
}
