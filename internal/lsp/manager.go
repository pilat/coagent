package lsp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/shellenv"
)

// lspInitTimeout bounds the initialize handshake so a server that spawns but never
// answers can't hold a start open (var: tests shrink it).
var lspInitTimeout = 30 * time.Second

// Manager defines the interface for LSP operations.
//
//nolint:interfacebloat // one method per LSP capability we expose; the count tracks the protocol, not our design
type Manager interface {
	Definition(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	References(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	Hover(ctx context.Context, workDir, file string, line, character int) (*Hover, error)
	DocumentSymbol(ctx context.Context, workDir, file string) ([]DocumentSymbol, error)
	WorkspaceSymbol(ctx context.Context, workDir, query string) ([]SymbolInformation, error)
	Implementation(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	PrepareCallHierarchy(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyItem, error)
	IncomingCalls(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyIncomingCall, error)
	OutgoingCalls(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyOutgoingCall, error)
	TouchFile(ctx context.Context, workDir, file string) error
	GetDiagnostics(ctx context.Context, workDir, file string) ([]Diagnostic, error)
	GetAllDiagnostics(ctx context.Context, workDir string, maxErrorsPerFile, maxFiles int) []FileDiagnostics
	Close()
}

var _ Manager = (*manager)(nil)

type manager struct {
	coagentBin string
	servers    []serverConfig
	clients    map[string]*client // key: "serverID:root"
	keyLocks   sync.Map           // map[key]*sync.Mutex — per-root spawn dedupe without holding mu
	provider   shellenv.Provider  // per-cwd shell activation; may be nil (fallback)
	mu         sync.RWMutex
	closed     bool
}

// NewManager creates a new LSP manager. provider may be nil: servers then spawn
// with the daemon's inherited env instead of the project's activated toolchain.
func NewManager(provider shellenv.Provider) Manager {
	coagentBin := coagentBin()
	if coagentBin == "" {
		logger.Named("lsp.manager").Warn("coagent bin dir unresolvable; LSP auto-install disabled")
	}

	return &manager{
		coagentBin: coagentBin,
		servers:    defaultServers(coagentBin),
		clients:    make(map[string]*client),
		provider:   provider,
	}
}

func (m *manager) TouchFile(ctx context.Context, workDir, file string) error {
	cl, err := m.getClient(ctx, workDir, file)
	if err != nil {
		return nil //nolint:nilerr // LSP is optional; if unavailable, skip silently
	}

	return cl.syncFile(ctx, file)
}

// Close stops all cached LSP clients. Clients are collected under mu and stopped
// outside it, so one slow stop can't serialize teardown or block getClient.
func (m *manager) Close() {
	m.mu.Lock()
	m.closed = true
	clients := m.clients
	m.clients = make(map[string]*client)
	m.mu.Unlock()

	for _, cl := range clients {
		_ = cl.stop()
	}
}

func (m *manager) getClient(ctx context.Context, workDir, file string) (*client, error) {
	ext := strings.ToLower(filepath.Ext(file))
	var server *serverConfig

	for i := range m.servers {
		if slices.Contains(m.servers[i].Extensions, ext) {
			server = &m.servers[i]
		}

		if server != nil {
			break
		}
	}

	if server == nil {
		return nil, fmt.Errorf("no LSP server for extension %s", ext)
	}

	root, err := server.RootFinder(workDir, file)
	if err != nil {
		return nil, fmt.Errorf("find root: %w", err)
	}

	key := fmt.Sprintf("%s:%s", server.ID, root)

	m.mu.RLock()
	cl, ok := m.clients[key]
	m.mu.RUnlock()

	if ok {
		return cl, nil
	}

	return m.startClient(ctx, server, root, key)
}

// startClient spawns and initializes a server under a per-key lock so concurrent
// callers for the same root dedupe the spawn while different roots proceed — and
// mu is never held across the spawn/install/handshake, so Close can't be starved.
func (m *manager) startClient(ctx context.Context, server *serverConfig, root, key string) (*client, error) {
	unlock := m.lockKey(key)
	defer unlock()

	// Re-check: another caller may have started this key while we waited.
	m.mu.RLock()
	cl, ok := m.clients[key]
	closed := m.closed
	m.mu.RUnlock()

	if ok {
		return cl, nil
	}

	if closed {
		return nil, errors.New("lsp manager closed")
	}

	logger.Ctx(ctx).Debug("starting LSP server", zap.String("server", server.ID), zap.String("root", root))

	cmd, err := server.Spawn(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", server.ID, err)
	}

	// Re-spawn through the provider so the server inherits root's activated
	// toolchain (PATH/GOROOT). WrapExec sets Dir=root — benign, gopls uses rootUri.
	if m.provider != nil {
		cmd, err = m.provider.WrapExec(ctx, root, cmd.Args, nil)
		if err != nil {
			return nil, fmt.Errorf("wrap %s spawn: %w", server.ID, err)
		}
	}

	initCtx, cancel := context.WithTimeout(ctx, lspInitTimeout)
	defer cancel()

	cl = newClient()
	if err := cl.startWithCommand(initCtx, cmd, root); err != nil {
		_ = cl.stop() //nolint:contextcheck // stop owns its bounded teardown ctx; don't leak the process

		return nil, fmt.Errorf("start client: %w", err)
	}

	m.mu.Lock()

	if m.closed {
		m.mu.Unlock()

		_ = cl.stop() //nolint:contextcheck // stop owns its bounded teardown ctx; reap the orphan outside mu

		return nil, errors.New("lsp manager closed")
	}

	m.clients[key] = cl
	m.mu.Unlock()

	return cl, nil
}

// lockKey serializes starts of the same key without holding mu across the spawn.
func (m *manager) lockKey(key string) func() {
	actual, _ := m.keyLocks.LoadOrStore(key, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	mu.Lock()

	return mu.Unlock
}
