package mcp

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/shellenv"
)

// AcquireForWorkDir builds per-workdir MCP access from already-resolved definitions,
// pooled when a pool is given. provider is used only on the direct path.
//
//nolint:nilnil // nil,nil means "no MCP configured", not failure; the only caller (tool/builtin) already checks Service != nil
func AcquireForWorkDir(
	ctx context.Context,
	pool Pool,
	servers map[string]ServerConfig,
	workDir string,
	provider shellenv.Provider,
) (Service, error) {
	configs := stampWorkDir(servers, workDir)
	if len(configs) == 0 {
		return nil, nil
	}

	if pool != nil {
		clients, hashes, err := pool.Acquire(ctx, configs)
		if err != nil {
			return nil, fmt.Errorf("pool acquire: %w", err)
		}

		return newPoolView(pool, clients, hashes), nil
	}

	return startDirect(ctx, workDir, configs, provider)
}

// stampWorkDir binds caller-supplied definitions to this session's workdir, which
// is part of the pool's identity hash. Callers leave WorkDir empty.
func stampWorkDir(servers map[string]ServerConfig, workDir string) map[string]ServerConfig {
	configs := make(map[string]ServerConfig, len(servers))

	for name, server := range servers {
		if !server.IsEnabled() {
			continue
		}

		server.WorkDir = workDir
		configs[name] = server
	}

	return configs
}

// startDirect creates a per-workdir MCP manager (no pool) and starts its servers.
// Server start failures are logged, not fatal — the manager is still returned.
func startDirect(
	ctx context.Context,
	workDir string,
	configs map[string]ServerConfig,
	provider shellenv.Provider,
) (Service, error) {
	log := logger.Ctx(ctx).Named("mcp.acquire")
	mgr := New(workDir, provider)

	stats, err := mgr.Start(ctx, &Config{Servers: configs})
	if err != nil {
		log.Warn("session_mcp_start_failed", zap.String("workdir", workDir), zap.Error(err))
	} else if stats != nil {
		log.Info(
			"session_mcp_started",
			zap.String("workdir", workDir),
			zap.Int("started", stats.Started),
			zap.Int("failed", stats.Failed),
		)
	}

	return mgr, nil
}
