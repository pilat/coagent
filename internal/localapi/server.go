package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	closeGrace      = 2 * time.Second
	maxRequestBytes = 8 << 20
)

// Server owns the local API listeners and bounds shutdown of their calls.
type Server struct {
	statusListener   net.Listener
	apiListener      net.Listener
	loopbackListener net.Listener
	status           *http.Server
	api              *http.Server
	loopback         *http.Server
	cancel           context.CancelFunc
	ready            atomic.Bool
}

// Client binds a token hash to the capability its author may exercise.
type Client struct {
	ID, TokenSHA256 string
	Control         controllerapi.Controller
}

// New serves only daemon status until an authenticated API listener is attached.
func New(ctx context.Context, statusListener net.Listener, source StatusSource) *Server {
	base, cancel := context.WithCancel(ctx)
	s := &Server{statusListener: statusListener, cancel: cancel}
	server := connect.NewServer()
	registerAnonymous(server, &daemonService{
		source: source, bootID: newBootID(), started: time.Now(), ready: &s.ready,
	})

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(maxRequestBytes))
	s.status = newHTTPServer(base, mux)
	s.api = newHTTPServer(base, nil)
	s.loopback = newHTTPServer(base, nil)

	return s
}

// AttachAPI binds authenticated authors before ServeAPI starts.
func (s *Server) AttachAPI(listener net.Listener, entries []Client) error {
	clients := make([]*client, 0, len(entries))
	for _, entry := range entries {
		author, err := bindClient(entry)
		if err != nil {
			return err
		}

		clients = append(clients, author)
	}

	server := connect.NewServer()
	register(server, s.status.BaseContext(listener).Done())

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(maxRequestBytes))

	s.apiListener = listener
	s.api.Handler = authenticate(clients, mux)

	return nil
}

// AttachLoopback requires a TCP listener and a successfully attached API handler.
func (s *Server) AttachLoopback(listener net.Listener, origins []string) {
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	hosts := []string{address, "localhost:" + port}
	s.loopbackListener = listener
	s.loopback.Handler = browserGuard(hosts, origins, s.api.Handler)
}

// ServeStatus returns nil on an orderly shutdown through Close.
func (s *Server) ServeStatus() error {
	return serve(s.status, s.statusListener)
}

// ServeAPI returns immediately when no API listener is attached.
func (s *Server) ServeAPI() error {
	if s.apiListener == nil {
		return nil
	}

	return serve(s.api, s.apiListener)
}

// ServeLoopback returns immediately when unattached, or nil on orderly shutdown.
func (s *Server) ServeLoopback() error {
	if s.loopbackListener == nil {
		return nil
	}

	return serve(s.loopback, s.loopbackListener)
}

// MarkReady makes status available after the managers have started.
func (s *Server) MarkReady() {
	s.ready.Store(true)
}

// Close drops status connections immediately; only API calls need time to drain.
func (s *Server) Close(ctx context.Context) error {
	s.cancel()
	_ = s.status.Close()
	_ = s.statusListener.Close()

	if s.apiListener == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, closeGrace)
	defer cancel()

	for _, server := range []*http.Server{s.api, s.loopback} {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}

	_ = s.apiListener.Close()
	if s.loopbackListener != nil {
		_ = s.loopbackListener.Close()
	}

	return nil
}

func newHTTPServer(base context.Context, handler http.Handler) *http.Server {
	return &http.Server{
		// net/http would otherwise answer OPTIONS * before the token check.
		DisableGeneralOptionsHandler: true,
		Handler:                      handler,
		ReadHeaderTimeout:            10 * time.Second,
		BaseContext:                  func(net.Listener) context.Context { return base },
	}
}

func serve(server *http.Server, listener net.Listener) error {
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("serve local API: %w", err)
	}

	return nil
}

func registerAnonymous(server *connect.Server, status *daemonService) {
	var daemon coagentv1connect.DaemonServiceHandler = status

	coagentv1connect.RegisterDaemonServiceHandler(server, daemon)
}

func register(server *connect.Server, closing <-chan struct{}) {
	var sessions coagentv1connect.SessionServiceHandler = sessionService{closing: closing}
	var outputs coagentv1connect.OutputServiceHandler = outputService{}
	var chats coagentv1connect.ChatServiceHandler = chatService{}
	var discovery coagentv1connect.DiscoveryServiceHandler = discoveryService{}

	coagentv1connect.RegisterSessionServiceHandler(server, sessions)
	coagentv1connect.RegisterOutputServiceHandler(server, outputs)
	coagentv1connect.RegisterChatServiceHandler(server, chats)
	coagentv1connect.RegisterDiscoveryServiceHandler(server, discovery)
}
