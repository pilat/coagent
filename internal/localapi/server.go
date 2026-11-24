package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
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

// Server owns the local API listener and bounds shutdown of its calls.
type Server struct {
	listener net.Listener
	http     *http.Server
	cancel   context.CancelFunc
}

// Client binds a token hash to the capability its author may exercise.
type Client struct {
	ID, TokenSHA256 string
	Control         controllerapi.Controller
}

// New binds authenticated authors to their supplied capabilities.
func New(ctx context.Context, listener net.Listener, entries []Client) (*Server, error) {
	clients := make([]*client, 0, len(entries))
	for _, entry := range entries {
		author, err := bindClient(entry)
		if err != nil {
			return nil, err
		}

		clients = append(clients, author)
	}

	base, cancel := context.WithCancel(ctx)

	server := connect.NewServer()

	register(server, base.Done())

	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(maxRequestBytes))

	return &Server{
		listener: listener,
		cancel:   cancel,
		http: &http.Server{
			// net/http would otherwise answer OPTIONS * before the token check.
			DisableGeneralOptionsHandler: true,
			Handler:                      authenticate(clients, mux),
			ReadHeaderTimeout:            10 * time.Second,
			BaseContext:                  func(net.Listener) context.Context { return base },
		},
	}, nil
}

// Serve returns nil on an orderly shutdown through Close.
func (s *Server) Serve() error {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("serve local API: %w", err)
	}

	return nil
}

// Close bounds shutdown even when a client stops reading its tail.
func (s *Server) Close(ctx context.Context) error {
	s.cancel()

	ctx, cancel := context.WithTimeout(ctx, closeGrace)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close()
	}

	return nil
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
