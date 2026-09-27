package mcp

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

// AcquireForWorkDir starts the enabled MCP servers for one tool stack.
// provider prepares the activated environment and runner confines each process.
//
//nolint:nilnil // nil,nil means "no MCP configured", not failure; the only caller (tool/builtin) already checks Service != nil
func AcquireForWorkDir(
	ctx context.Context,
	servers map[string]ServerConfig,
	workDir string,
	provider shellenv.Provider,
	runner procexec.Runner,
) (Service, error) {
	configs := stampWorkDir(servers, workDir)
	if len(configs) == 0 {
		return nil, nil
	}

	return startDirect(ctx, workDir, configs, provider, runner)
}

// stampWorkDir binds caller-supplied definitions to this stack's workdir.
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

// startDirect creates a per-workdir MCP manager and starts its servers.
// Server start failures are logged, not fatal — the manager is still returned.
func startDirect(
	ctx context.Context,
	workDir string,
	configs map[string]ServerConfig,
	provider shellenv.Provider,
	runner procexec.Runner,
) (Service, error) {
	log := logger.Ctx(ctx).Named("mcp.acquire")
	mgr := New(workDir, provider, runner)

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
