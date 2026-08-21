package lsp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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

type Manager interface {
	Definition(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	References(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	Hover(ctx context.Context, workDir, file string, line, character int) (*Hover, error)
	DocumentSymbol(ctx context.Context, workDir, file string) ([]DocumentSymbol, error)
	WorkspaceSymbol(ctx context.Context, workDir, file, query string) ([]SymbolInformation, error)
	Implementation(ctx context.Context, workDir, file string, line, character int) ([]Location, error)
	PrepareCallHierarchy(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyItem, error)
	IncomingCalls(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyIncomingCall, error)
	OutgoingCalls(ctx context.Context, workDir, file string, line, character int) ([]CallHierarchyOutgoingCall, error)
	GetDiagnostics(ctx context.Context, workDir, file string) ([]Diagnostic, error)
	GetAllDiagnostics(ctx context.Context, workDir string, maxErrorsPerFile, maxFiles int) []FileDiagnostics
	Close()
}

var _ Manager = (*manager)(nil)

type manager struct {
	servers  []serverConfig
	clients  map[clientKey]*client
	keyLocks sync.Map          // map[key]*sync.Mutex — per-root spawn dedupe without holding mu
	provider shellenv.Provider // per-cwd shell activation; may be nil (fallback)
	mu       sync.RWMutex
	closed   bool
}

type clientKey struct {
	serverID string
	root     string
}

func (k clientKey) String() string { return k.serverID + ":" + k.root }

// NewManager creates a new LSP manager. provider may be nil: servers then spawn
// with the daemon's inherited env instead of the project's activated toolchain.
func NewManager(provider shellenv.Provider) Manager {
	return &manager{
		servers:  defaultServers(),
		clients:  make(map[clientKey]*client),
		provider: provider,
	}
}

// Close stops all cached LSP clients. Clients are collected under mu and stopped
// outside it, so one slow stop can't serialize teardown or block getClient.
func (m *manager) Close() {
	m.mu.Lock()
	m.closed = true
	clients := m.clients
	m.clients = make(map[clientKey]*client)
	m.mu.Unlock()

	for _, cl := range clients {
		_ = cl.stop(context.Background())
	}
}

func (m *manager) getClient(ctx context.Context, workDir, file string) (*client, error) {
	identity, err := resolveFile(workDir, file)
	if err != nil {
		return nil, err
	}

	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve work directory: %w", err)
	}

	workDir = filepath.Clean(workDir)

	server := m.serverFor(identity.path)
	if server == nil {
		return nil, fmt.Errorf("no LSP server for file %s", file)
	}

	root, err := server.RootFinder(workDir, identity.path)
	if err != nil {
		return nil, fmt.Errorf("find root: %w", err)
	}

	key := clientKey{serverID: server.ID, root: root}

	m.mu.RLock()
	cl, ok := m.clients[key]
	m.mu.RUnlock()

	if ok && !cl.hasExited() {
		return cl, nil
	}

	if ok {
		m.evictClient(ctx, key, cl)
	}

	return m.startClient(ctx, server, root, server.languageID(identity.path), key)
}

func (m *manager) serverFor(file string) *serverConfig {
	for i := range m.servers {
		if m.servers[i].matches(file) {
			return &m.servers[i]
		}
	}

	return nil
}

// startClient spawns and initializes a server under a per-key lock so concurrent
// callers for the same root dedupe the spawn while different roots proceed — and
// mu is never held across the spawn/handshake, so Close can't be starved.
func (m *manager) startClient(
	ctx context.Context,
	server *serverConfig,
	root, languageID string,
	key clientKey,
) (*client, error) {
	unlock := m.lockKey(key.String())
	defer unlock()

	if cl, found, err := m.cachedClient(ctx, key); found || err != nil {
		return cl, err
	}

	cl, err := m.createClient(ctx, server, root, languageID, key)
	if err != nil {
		return nil, err
	}

	return m.cacheClient(ctx, key, cl)
}

func (m *manager) cachedClient(ctx context.Context, key clientKey) (*client, bool, error) {
	m.mu.RLock()
	cl, ok := m.clients[key]
	closed := m.closed
	m.mu.RUnlock()

	if ok && !cl.hasExited() {
		return cl, true, nil
	}

	if ok {
		m.evictClient(ctx, key, cl)
	}

	if closed {
		return nil, false, errors.New("lsp manager closed")
	}

	return nil, false, nil
}

func (m *manager) createClient(
	ctx context.Context,
	server *serverConfig,
	root, languageID string,
	key clientKey,
) (*client, error) {
	logger.Ctx(ctx).Debug("starting LSP server", zap.String("server", server.ID), zap.String("root", root))

	cmd, err := m.wrappedServerCommand(ctx, server, root)
	if err != nil {
		return nil, err
	}

	initCtx, cancel := context.WithTimeout(ctx, lspInitTimeout)
	defer cancel()

	cl := newClient()
	cl.languageID = languageID

	cl.onExit = func() { m.evictClient(context.Background(), key, cl) } //nolint:contextcheck // process exit has no request owner.
	if err := cl.startWithCommand(initCtx, cmd, root); err != nil {
		_ = cl.stop(ctx)
		return nil, fmt.Errorf("start client: %w", err)
	}

	return cl, nil
}

func (m *manager) cacheClient(ctx context.Context, key clientKey, cl *client) (*client, error) {
	m.mu.Lock()
	if !m.closed {
		m.clients[key] = cl
		m.mu.Unlock()

		return cl, nil
	}
	m.mu.Unlock()

	_ = cl.stop(ctx)

	return nil, errors.New("lsp manager closed")
}

// lockKey serializes starts of the same key without holding mu across the spawn.
func (m *manager) lockKey(key string) func() {
	actual, _ := m.keyLocks.LoadOrStore(key, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	mu.Lock()

	return mu.Unlock
}

func (m *manager) evictClient(ctx context.Context, key clientKey, candidate *client) {
	m.mu.Lock()
	removed := false

	if m.clients[key] == candidate {
		delete(m.clients, key)

		removed = true
	}
	m.mu.Unlock()

	if removed && candidate.processDone != nil {
		go func() { _ = candidate.stop(ctx) }()
	}
}
