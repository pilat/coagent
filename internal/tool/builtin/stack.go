package builtin

import (
	"context"
	"fmt"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// StackConfig configures a session-scoped local tool stack.
type StackConfig struct {
	WorkDir         string
	RepoRoot        string                      // main repository path for worktree sessions; empty otherwise
	Pool            mcp.Pool                    // may be nil
	Servers         map[string]mcp.ServerConfig // resolved MCP definitions; empty = no MCP
	Unified         *config.UnifiedConfig       // for the Bash sandbox config
	Loader          loader.Service
	Todo            todo.Service
	TodoReplacement TodoReplacement
	Provider        shellenv.Provider // per-cwd shell activation; may be nil (fallback)
}

// Stack is a session-scoped local tool set. It owns the LSP manager and MCP access
// it creates; callers MUST Close() it on every exit path.
type Stack struct {
	Registry tool.Registry
	lspMgr   lsp.Manager
	mcpSvc   mcp.Service // nil when no MCP servers configured
}

// BuildStack assembles the local tool registry: core builtins plus MCP tools.
func BuildStack(ctx context.Context, cfg StackConfig) (*Stack, error) {
	sandboxCfg := bashSandboxConfig(cfg)

	//nolint:contextcheck // Sandbox preflight is a bounded process-wide self-test.
	bashRunner, err := bashsandbox.New(sandboxCfg, cfg.Provider)
	if err != nil {
		return nil, fmt.Errorf("create bash sandbox: %w", err)
	}

	mutator, err := newFileMutator(sandboxCfg.Enabled, bashRunner)
	if err != nil {
		return nil, fmt.Errorf("create file mutator: %w", err)
	}

	registry := tool.NewRegistry()
	lspMgr := lsp.NewManager(cfg.Provider)

	registerCoreTools(registry, cfg.WorkDir, cfg.Loader, cfg.Todo, cfg.TodoReplacement, lspMgr, bashRunner, mutator)

	// MCP failure degrades to a builtin-only stack: a broken MCP server must not block sessions.
	mcpSvc, err := mcp.AcquireForWorkDir(ctx, cfg.Pool, cfg.Servers, cfg.WorkDir, cfg.Provider)
	if err != nil {
		logger.Ctx(ctx).Warn("mcp_acquire_failed", zap.Error(err))
	}

	if mcpSvc != nil {
		mcpSvc.RegisterTools(registry)
	}

	return &Stack{Registry: registry, lspMgr: lspMgr, mcpSvc: mcpSvc}, nil
}

// Close releases the LSP manager and MCP access the stack owns.
func (s *Stack) Close() error {
	if s.lspMgr != nil {
		s.lspMgr.Close()
	}

	if s.mcpSvc != nil {
		s.mcpSvc.Stop()
	}

	return nil
}

// registerCoreTools registers the filesystem/shell/skill builtins.
// Registration order feeds the LLM prompt-cache key — do not reorder.
func registerCoreTools(
	registry tool.Registry,
	workDir string,
	ldr loader.Service,
	todoSvc todo.Service,
	todoReplacement TodoReplacement,
	lspMgr lsp.Manager,
	bashRunner bashsandbox.Runner,
	fileMutator fileMutator,
) {
	registry.Register(newReadTool(workDir))
	registry.Register(newWriteTool(workDir, lspMgr, fileMutator))
	registry.Register(newEditTool(workDir, lspMgr, fileMutator))
	registry.Register(newApplyPatchTool(workDir, fileMutator))

	registry.Register(NewLsTool(workDir))
	registry.Register(newGlobTool(workDir))
	registry.Register(newGrepTool(workDir))

	registry.Register(newBashTool(workDir, bashRunner))

	registry.Register(newWebFetchTool())

	registry.Register(NewSkillTool(ldr))
	registry.Register(newTodoReadTool(todoSvc))
	registry.Register(newTodoWriteTool(todoSvc, todoReplacement))

	registry.Register(NewBatchTool(registry))

	registry.Register(newLspTool(workDir, lspMgr))
}

func bashSandboxConfig(cfg StackConfig) bashsandbox.Config {
	sandboxCfg := bashsandbox.Config{WorkDir: cfg.WorkDir}
	if cfg.Unified == nil {
		return sandboxCfg
	}

	sandboxCfg.Enabled = cfg.Unified.Tools.Bash.Sandbox.Enabled
	sandboxCfg.WritablePaths = cfg.Unified.Tools.Bash.Sandbox.WritablePaths

	// A linked work tree shares the object store and refs with the main
	// repository, so git mutations must reach the main .git; the checkout
	// of the main work tree stays read-only.
	if cfg.RepoRoot != "" {
		sandboxCfg.WritablePaths = append(sandboxCfg.WritablePaths, filepath.Join(cfg.RepoRoot, ".git"))
	}

	return sandboxCfg
}
