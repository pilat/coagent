package builtin

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// StackConfig configures a session-scoped local tool stack.
type StackConfig struct {
	SessionID       int64
	WorkDir         string
	RepoRoot        string                      // main repository path for worktree sessions; empty otherwise
	Pool            mcp.Pool                    // may be nil
	Servers         map[string]mcp.ServerConfig // resolved MCP definitions; empty = no MCP
	Unified         *config.UnifiedConfig       // for the sandbox config
	Loader          loader.Service
	Todo            todo.Service
	TodoReplacement TodoReplacement
	Provider        shellenv.Provider // per-cwd shell activation; may be nil (fallback)
	ShieldsUp       bool
}

// Stack is a session-scoped local tool set. It owns the LSP manager and MCP access
// it creates; callers MUST Close() it on every exit path.
type Stack struct {
	Registry tool.Registry
	lspMgr   lsp.Manager
	mcpSvc   mcp.Service // nil when no MCP servers configured
	runner   bashsandbox.Runner
	access   safefile.Access
}

// BuildStack assembles the local tool registry: core builtins plus MCP tools.
//
//nolint:wsl_v5 // All tools must receive the same runner and rooted-access policy.
func BuildStack(ctx context.Context, cfg StackConfig) (*Stack, error) {
	provider := cfg.Provider
	if cfg.ShieldsUp {
		provider = nil
	}

	scope := safefile.HostReadable
	if cfg.ShieldsUp {
		scope = safefile.ProjectConfined
	}
	access, err := safefile.New(cfg.WorkDir, scope)
	if err != nil {
		return nil, fmt.Errorf("create filesystem access: %w", err)
	}
	if cfg.Loader != nil {
		cfg.Loader.SetProjectAccess(access)
	}
	sandboxCfg := bashSandboxConfig(cfg)
	sandboxCfg.CanonicalWorkDir = access.CanonicalRoot()

	//nolint:contextcheck // Sandbox preflight is a bounded process-wide self-test.
	bashRunner, err := bashsandbox.New(sandboxCfg, provider)
	if err != nil {
		_ = access.Close()

		return nil, fmt.Errorf("create bash sandbox: %w", err)
	}

	mutator, err := newFileMutator(sandboxCfg.Enabled, bashRunner)
	if err != nil {
		_ = access.Close()

		return nil, fmt.Errorf("create file mutator: %w", err)
	}

	registry := tool.NewRegistry()
	processRunner := procexec.Runner(bashRunner)
	lspMgr := lsp.NewManagerWithAccess(provider, processRunner, access)

	registerCoreTools(
		registry,
		cfg.WorkDir,
		access,
		cfg.Loader,
		cfg.Todo,
		cfg.TodoReplacement,
		lspMgr,
		bashRunner,
		mutator,
		cfg.Unified,
	)

	// MCP failure degrades to a builtin-only stack: a broken MCP server must not block sessions.
	mcpSvc, err := mcp.AcquireForWorkDir(ctx, cfg.Pool, cfg.Servers, cfg.WorkDir, provider, processRunner)
	if err != nil {
		logger.Ctx(ctx).Warn("mcp_acquire_failed", zap.Error(err))
	}

	if mcpSvc != nil {
		mcpSvc.RegisterTools(registry)
	}

	return &Stack{
		Registry: registry, lspMgr: lspMgr, mcpSvc: mcpSvc, runner: bashRunner, access: access,
	}, nil
}

// ProcessPolicyKey returns the identity of the process policy built for this stack.
func (s *Stack) ProcessPolicyKey() string {
	if s == nil || s.runner == nil {
		return ""
	}

	return s.runner.PolicyKey()
}

// Close releases the LSP manager and MCP access the stack owns.
//
//nolint:wsl_v5 // Close preserves subsystem order while joining independent failures.
func (s *Stack) Close() error {
	if s.lspMgr != nil {
		s.lspMgr.Close()
	}

	if s.mcpSvc != nil {
		s.mcpSvc.Stop()
	}
	if s.access != nil {
		if err := s.access.Close(); err != nil {
			return fmt.Errorf("close filesystem access: %w", err)
		}
	}

	return nil
}

// registerCoreTools registers the filesystem/shell/skill builtins. Registration
// order never reaches the wire — List() sorts by ID and that sorted membership
// is what feeds the LLM prompt-cache key.
func registerCoreTools(
	registry tool.Registry,
	workDir string,
	access safefile.Access,
	ldr loader.Service,
	todoSvc todo.Service,
	todoReplacement TodoReplacement,
	lspMgr lsp.Manager,
	bashRunner bashsandbox.Runner,
	fileMutator fileMutator,
	unified *config.UnifiedConfig,
) {
	registry.Register(newReadToolWithAccess(workDir, access))
	registry.Register(newWriteToolWithAccess(workDir, access, lspMgr, fileMutator))
	registry.Register(newEditToolWithAccess(workDir, access, lspMgr, fileMutator))
	registry.Register(newApplyPatchToolWithAccess(workDir, access, fileMutator))

	registry.Register(newLsTool(workDir, access))
	registry.Register(newGlobToolWithAccess(workDir, access))
	registry.Register(newGrepToolWithAccess(workDir, access))

	registry.Register(newBashTool(workDir, bashRunner))

	registry.Register(newWebFetchTool())

	registry.Register(NewSkillTool(ldr))
	registry.Register(newTodoReadTool(todoSvc))
	registry.Register(newTodoWriteTool(todoSvc, todoReplacement))

	registry.Register(NewBatchTool(registry))

	registry.Register(newLspTool(workDir, lspMgr))

	// Conditional registration: a model never sees a search tool that always
	// errors. With no tools.search config the tool is absent from the registry
	// and from the prompt.
	if searchTool := newSearchToolFromConfig(unified); searchTool != nil {
		registry.Register(searchTool)
	}
}

// newSearchToolFromConfig builds the builtin websearch tool when the unified
// config selects a provider, nil otherwise. Registration is skipped entirely
// for an unconfigured or disabled section.
func newSearchToolFromConfig(unified *config.UnifiedConfig) tool.Tool {
	if unified == nil {
		return nil
	}

	s := unified.Tools.Search
	if !s.SearchActive() {
		return nil
	}

	client := &http.Client{
		Timeout:   webFetchTimeout,
		Transport: newRestrictedTransport(),
	}

	var provider searchProvider

	switch s.Provider {
	case config.SearchProviderTavily:
		provider = &tavilySearchProvider{client: client, apiKey: s.APIKey}
	case config.SearchProviderSearxng:
		provider = &searxngSearchProvider{client: client, baseURL: s.BaseURL}
	default:
		// Config validation rejects unknown providers before this point.
		return nil
	}

	return newWebSearchTool(provider, s.MaxResults)
}

//nolint:wsl_v5 // Unified sandbox defaults are normalized at one boundary.
func bashSandboxConfig(cfg StackConfig) bashsandbox.Config {
	sandboxCfg := bashsandbox.Config{WorkDir: cfg.WorkDir, SessionKey: fmt.Sprintf("session:%d", cfg.SessionID)}
	if cfg.ShieldsUp {
		sandboxCfg.ReadScope = bashsandbox.ProjectConfined
	}
	if cfg.Unified == nil {
		return sandboxCfg
	}

	sandboxCfg.Enabled = cfg.Unified.Sandbox.Enabled
	sandboxCfg.WritablePaths = cfg.Unified.Sandbox.WritablePaths

	// A linked work tree shares the object store and refs with the main
	// repository, so git mutations must reach the main .git; the checkout
	// of the main work tree stays read-only.
	if cfg.RepoRoot != "" && !cfg.ShieldsUp {
		sandboxCfg.WritablePaths = append(sandboxCfg.WritablePaths, filepath.Join(cfg.RepoRoot, ".git"))
	}

	return sandboxCfg
}
