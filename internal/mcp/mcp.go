package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/tool"
)

const defaultMCPStartTimeout = 2 * time.Minute

type ServerStats struct {
	Total   int
	Started int
	Failed  int
	Skipped int
}

// Service manages multiple MCP server connections.
type Service interface {
	Start(ctx context.Context, config *Config) (*ServerStats, error)
	Stop()
	RegisterTools(registry tool.Registry) int
	GetClient(name string) *Client
	Stats() ServerStats
}

var _ Service = (*svc)(nil)

type svc struct {
	mu       sync.RWMutex
	clients  map[string]*Client
	workDir  string
	provider shellenv.Provider
	stats    ServerStats
}

// New creates a new MCP manager. provider (may be nil) routes each server spawn
// through workDir shell activation.
func New(workDir string, provider shellenv.Provider) Service {
	return &svc{
		clients:  make(map[string]*Client),
		workDir:  workDir,
		provider: provider,
	}
}

func (s *svc) Start(ctx context.Context, config *Config) (*ServerStats, error) {
	log := logger.Ctx(ctx).Named("mcp.service")

	if config == nil {
		log.Debug("no_config")
		return &ServerStats{}, nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	started := 0
	failed := 0
	skipped := 0

	for name, cfg := range config.Servers {
		if !cfg.IsEnabled() {
			log.Info("server_disabled", zap.String("name", name))

			skipped++

			continue
		}

		wg.Add(1)

		go func(name string, cfg ServerConfig) {
			defer wg.Done()

			if err := s.startServer(ctx, name, cfg); err != nil {
				mu.Lock()

				failed++
				mu.Unlock()
				log.Warn("server_failed", zap.String("name", name), zap.Error(err))
			} else {
				mu.Lock()
				started++
				mu.Unlock()
				log.Info("server_started", zap.String("name", name))
			}
		}(name, cfg)
	}

	wg.Wait()

	total := started + failed + skipped
	s.stats = ServerStats{
		Total:   total,
		Started: started,
		Failed:  failed,
		Skipped: skipped,
	}

	if started == 0 && total > 0 {
		log.Warn("all_servers_failed", zap.Int("total", total))
	} else if failed > 0 {
		log.Info("partial_servers", zap.Int("started", started), zap.Int("failed", failed), zap.Int("total", total))
	} else if started > 0 {
		log.Info("all_servers_started", zap.Int("count", started), zap.Int("skipped", skipped))
	}

	return &s.stats, nil
}

func (s *svc) Stop() {
	log := logger.Named("mcp.service")

	s.mu.Lock()
	clients := s.clients
	s.clients = make(map[string]*Client)
	s.mu.Unlock()

	// Close outside s.mu — a bounded-but-nonzero Close must not serialize behind
	// the lock that RegisterTools/GetClient also take.
	for name, client := range clients {
		if err := client.Close(); err != nil {
			log.Warn("close_client_failed", zap.String("name", name), zap.Error(err))
		}
	}
}

func (s *svc) RegisterTools(registry tool.Registry) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0

	for serverName, client := range s.clients {
		for toolName := range client.Tools() {
			mcpToolWrapper := newMCPTool(serverName, toolName, client)
			registry.Register(mcpToolWrapper)

			count++
		}
	}

	return count
}

func (s *svc) GetClient(name string) *Client {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.clients[name]
}

func (s *svc) Stats() ServerStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.stats
}

func (s *svc) startServer(ctx context.Context, name string, cfg ServerConfig) error {
	if cfg.WorkDir == "" {
		cfg.WorkDir = s.workDir
	}

	startCtx, cancel := context.WithTimeout(ctx, defaultMCPStartTimeout)
	defer cancel()

	type result struct {
		client *Client
		err    error
	}
	resultCh := make(chan result, 1)

	go func() {
		client, err := NewClient(startCtx, name, cfg, s.provider)
		// Use non-blocking send with select to avoid goroutine leak
		select {
		case resultCh <- result{client, err}:
		case <-startCtx.Done():
			// Context cancelled, cleanup client if created
			if client != nil {
				_ = client.Close()
			}
		}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return res.err
		}

		s.mu.Lock()
		s.clients[name] = res.client
		s.mu.Unlock()

		return nil

	case <-startCtx.Done():
		return fmt.Errorf("timeout after %v: %w", defaultMCPStartTimeout, startCtx.Err())
	}
}
