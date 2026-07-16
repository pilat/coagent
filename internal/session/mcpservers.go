package session

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
)

// resolveMCPServers expands the project's registry rows against the in-memory
// secrets. One unresolvable server is skipped, not fatal — the session still starts.
func resolveMCPServers(
	ctx context.Context,
	store mcpstore.Store,
	secrets config.Secrets,
	projectID int64,
) map[string]mcp.ServerConfig {
	if store == nil || projectID == 0 {
		return nil
	}

	log := logger.Ctx(ctx).Named("session.mcp")

	defs, err := store.ListForProject(ctx, projectID)
	if err != nil {
		log.Warn("mcp_registry_read_failed", zap.Int64("project", projectID), zap.Error(err))

		return nil
	}

	servers := make(map[string]mcp.ServerConfig, len(defs))

	for _, def := range defs {
		env, err := expandEnv(secrets, def.Env)
		if err != nil {
			// expand's error names the variable, never a value.
			log.Warn("mcp_server_skipped", zap.String("server", def.Name), zap.Error(err))

			continue
		}

		servers[def.Name] = mcp.ServerConfig{
			Command: def.Command,
			Args:    def.Args,
			Env:     env,
		}
	}

	return servers
}

func expandEnv(secrets config.Secrets, raw map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(raw))

	for key, value := range raw {
		expanded, err := secrets.Expand(value)
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", key, err)
		}

		env[key] = expanded
	}

	return env, nil
}
