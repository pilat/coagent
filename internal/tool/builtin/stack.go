package builtin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/lsp"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// StackConfig configures a session-scoped local tool stack.
type StackConfig struct {
	ProjectID       int64
	SessionID       int64
	RootSessionID   int64 // 0 = this session is its own root
	WorkDir         string
	RepoRoot        string                      // main repository path for worktree sessions; empty otherwise
	Servers         map[string]mcp.ServerConfig // resolved MCP definitions; empty = no MCP
	Unified         *config.UnifiedConfig       // for the sandbox config
	Loader          loader.Service
	Todo            todo.Service
	TodoReplacement TodoReplacement
	FileReadTracker FileReadTracker
	Provider        shellenv.Provider         // optional injected provider; nil creates a stack-owned provider
	Resources       Resources                 // optional session owner across activations
	ProcessService  backgroundprocess.Service // session-bound process lifecycle; may be nil
}

// Stack owns activation-local tools and leases session resources until Close.
type Stack struct {
	Registry  tool.Registry
	lspMgr    lsp.Manager
	mcpSvc    mcp.Service // nil when no MCP servers configured
	resources *resourceLease
	closeOnce sync.Once
	closeErr  error
	runner    bashsandbox.Runner
	access    safefile.Access
	sessionID int64
	web       []*http.Transport
}

// BuildStack assembles the local tool registry: core builtins plus MCP tools.
//
//nolint:funlen,wsl_v5 // All tools share one runner, rooted access and ordered cleanup.
func BuildStack(ctx context.Context, cfg StackConfig) (*Stack, error) {
	canonicalRoot, err := canonicalProjectRoot(cfg.WorkDir)
	if err != nil {
		if cfg.Resources != nil {
			_ = cfg.Resources.Retire(cfg.SessionID)
		}
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	cfg.WorkDir = canonicalRoot

	sandboxCfg, err := bashSandboxConfig(cfg, canonicalRoot)
	if err != nil {
		if cfg.Resources != nil {
			_ = cfg.Resources.Retire(cfg.SessionID)
		}
		return nil, fmt.Errorf("compile sandbox policy: %w", err)
	}
	var resources *resourceLease
	if cfg.Resources != nil {
		resources, err = cfg.Resources.acquire(cfg, sandboxCfg)
		if err != nil {
			return nil, fmt.Errorf("acquire session tool resources: %w", err)
		}
	} else {
		resources = newResourceLease(cfg)
	}
	provider := resources.provider
	closeProvider := func() {
		if cfg.Resources != nil {
			_ = cfg.Resources.Retire(cfg.SessionID)
		}
		_ = resources.release()
	}
	//nolint:contextcheck // Sandbox preflight is a bounded process-wide self-test.
	bashRunner, err := bashsandbox.New(sandboxCfg, provider)
	if err != nil {
		closeProvider()
		return nil, fmt.Errorf("create bash sandbox: %w", err)
	}

	// The runner materializes the declared read-write grants, so file access can
	// hold every present one. Both surfaces therefore share one compiled policy.
	access, err := safefile.New(sandboxCfg.Policy, cfg.WorkDir)
	if err != nil {
		closeProvider()
		return nil, fmt.Errorf("create filesystem access: %w", err)
	}

	mutator, err := newFileMutator(sandboxCfg.Enabled, bashRunner)
	if err != nil {
		_ = access.Close()
		closeProvider()

		return nil, fmt.Errorf("create file mutator: %w", err)
	}
	if sandboxCfg.Enabled {
		mutator = exactGrantFileMutator{access: access, fallback: mutator}
	}

	registry := tool.NewRegistry()
	processRunner := procexec.Runner(bashRunner)
	lspMgr := lsp.NewManagerWithAccess(provider, processRunner, access)
	processProjectDir := ""
	if cfg.ProjectID > 0 {
		processProjectDir, err = coagenthome.ProcessProjectDirName(cfg.ProjectID)
		if err != nil {
			_ = access.Close()
			closeProvider()

			return nil, fmt.Errorf("resolve process project directory: %w", err)
		}
	}

	webTransports := registerCoreTools(
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
		cfg.ProcessService,
		processProjectDir,
		cfg.SessionID,
		rootSessionID(cfg),
		cfg.FileReadTracker,
	)

	// MCP failure degrades to a builtin-only stack: a broken MCP server must not block sessions.
	mcpSvc, err := resources.acquireMCP(ctx, cfg, bashRunner)
	if err != nil {
		logger.Ctx(ctx).Warn("mcp_acquire_failed", zap.Error(err))
	}

	if mcpSvc != nil {
		mcpSvc.RegisterTools(registry)
	}

	logger.Ctx(ctx).Named("tool.stack").Info("stack_started",
		zap.Int64("session_id", cfg.SessionID), zap.Int64("root_id", rootSessionID(cfg)),
		zap.Bool("sandbox", sandboxCfg.Enabled))

	return &Stack{
		Registry: registry, lspMgr: lspMgr, mcpSvc: mcpSvc, runner: bashRunner, access: access,
		resources: resources, web: webTransports,
		sessionID: cfg.SessionID,
	}, nil
}

func stackProvider(cfg StackConfig) (shellenv.Provider, bool) {
	if cfg.Provider != nil {
		return cfg.Provider, false
	}

	return shellenv.New(), true
}

// Runner returns the stack's confinement runner, or nil when the stack has
// none. Callers route work through it instead of the host.
func (s *Stack) Runner() bashsandbox.Runner {
	if s == nil {
		return nil
	}

	return s.runner
}

// SandboxedGit returns a Git client that runs inside this stack's confinement,
// so project-dependent hooks, helpers and fsmonitor cannot execute outside the
// policy. It is nil when the stack has no runner, and the caller keeps the
// daemon's client then.
func (s *Stack) SandboxedGit() git.Client {
	if s == nil || s.runner == nil {
		return nil
	}

	// Upcast to the neutral process contract: the confinement type must not leak
	// into the git component's dependency graph.
	return git.NewSandboxed(procexec.Runner(s.runner))
}

// Access returns the stack's rooted filesystem authority, or nil when the stack
// has none.
func (s *Stack) Access() safefile.Access {
	if s == nil {
		return nil
	}

	return s.access
}

// Close releases activation-local tools and the session resource lease.
func (s *Stack) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *Stack) close() error {
	logger.Named("tool.stack").Info("stack_stopped",
		zap.Int64("session_id", s.sessionID))

	for _, transport := range s.web {
		transport.CloseIdleConnections()
	}

	if s.lspMgr != nil {
		s.lspMgr.Close()
	}

	var err error
	if s.access != nil {
		err = errors.Join(err, s.access.Close())
	}

	if s.resources != nil {
		err = errors.Join(err, s.resources.release())
	}

	return err
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
	processService backgroundprocess.Service,
	processProjectDir string,
	sessionID, rootID int64,
	tracker FileReadTracker,
) []*http.Transport {
	registry.Register(newReadToolWithAccess(workDir, access, tracker))
	registry.Register(newWriteToolWithAccess(workDir, access, lspMgr, fileMutator, tracker))
	registry.Register(newEditToolWithAccess(workDir, access, lspMgr, fileMutator, tracker))
	registry.Register(newApplyPatchToolWithAccess(workDir, access, fileMutator, tracker))

	registry.Register(newLsTool(workDir, access))
	registry.Register(newGlobToolWithAccess(workDir, access))
	registry.Register(newGrepToolWithAccess(workDir, access))

	registry.Register(newBashToolWithAccess(
		workDir,
		bashRunner,
		processService,
		processProjectDir,
		sessionID,
		rootID,
		access,
	))

	if processService != nil {
		registry.Register(newCancelProcessTool(processService, sessionID))
	}

	registry.Register(newTailTool(workDir, access))

	fetch := newWebFetchTool()
	registry.Register(fetch)
	transports := []*http.Transport{fetch.transport}

	registry.Register(NewSkillTool(ldr))
	registry.Register(newTodoReadTool(todoSvc))
	registry.Register(newTodoWriteTool(todoSvc, todoReplacement))

	registry.Register(NewBatchTool(registry))

	registry.Register(newLspTool(workDir, lspMgr))

	// Conditional registration: a model never sees a search tool that always
	// errors. With no tools.search config the tool is absent from the registry
	// and from the prompt.
	if searchTool, transport := newSearchToolFromConfig(unified); searchTool != nil {
		registry.Register(searchTool)

		transports = append(transports, transport)
	}

	return transports
}

func rootSessionID(cfg StackConfig) int64 {
	if cfg.RootSessionID != 0 {
		return cfg.RootSessionID
	}

	return cfg.SessionID
}

// newSearchToolFromConfig builds the builtin websearch tool when the unified
// config selects a provider, nil otherwise. Registration is skipped entirely
// for an unconfigured or disabled section.
func newSearchToolFromConfig(unified *config.UnifiedConfig) (tool.Tool, *http.Transport) {
	if unified == nil {
		return nil, nil
	}

	s := unified.Tools.Search
	if !s.SearchActive() {
		return nil, nil
	}

	transport := newRestrictedTransport()
	client := &http.Client{
		Timeout:   webFetchTimeout,
		Transport: transport,
	}

	var provider searchProvider

	switch s.Provider {
	case config.SearchProviderTavily:
		provider = &tavilySearchProvider{client: client, apiKey: s.APIKey}
	case config.SearchProviderSearxng:
		provider = &searxngSearchProvider{client: client, baseURL: s.BaseURL}
	default:
		// Config validation rejects unknown providers before this point.
		return nil, nil
	}

	return newWebSearchTool(provider, s.MaxResults), transport
}

// canonicalProjectRoot resolves the session's canonical project root: the
// policy's project identity and the file tools' read root.
func canonicalProjectRoot(workDir string) (string, error) {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve work dir: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %q: %w", abs, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", resolved, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("project %q is not a directory", workDir)
	}

	return resolved, nil
}

// bashSandboxConfig compiles the session's effective policy into the runner
// configuration from the operator's global and per-project rules.
func bashSandboxConfig(cfg StackConfig, canonicalRoot string) (bashsandbox.Config, error) {
	sessionKey := fmt.Sprintf("session:%d", cfg.SessionID)

	if cfg.Unified == nil || !cfg.Unified.Sandbox.Enabled {
		return bashsandbox.Config{WorkDir: cfg.WorkDir, SessionKey: sessionKey}, nil
	}

	sourceRepoRoot, err := canonicalSourceRepoRoot(cfg.RepoRoot)
	if err != nil {
		return bashsandbox.Config{}, err
	}

	projectRules, err := sandboxpolicy.SelectProjectRules(cfg.Unified.Sandbox, canonicalRoot, sourceRepoRoot)
	if err != nil {
		return bashsandbox.Config{}, fmt.Errorf("select project sandbox rules: %w", err)
	}

	outputRoot, err := prepareProcessOutputRoot(cfg.ProjectID)
	if err != nil {
		return bashsandbox.Config{}, err
	}

	compiled, err := sandboxpolicy.Compile(sandboxpolicy.Request{
		ProjectRoot:       canonicalRoot,
		ProjectID:         cfg.ProjectID,
		WorkDir:           cfg.WorkDir,
		GlobalRules:       cfg.Unified.Sandbox.Rules,
		ProjectRules:      projectRules,
		WorktreeGitDir:    worktreeGitDir(cfg.RepoRoot),
		ProcessOutputRoot: outputRoot,
	})
	if err != nil {
		return bashsandbox.Config{}, fmt.Errorf("compile sandbox policy: %w", err)
	}

	return bashsandbox.Config{
		Enabled: true, Policy: compiled, WorkDir: cfg.WorkDir, SessionKey: sessionKey,
	}, nil
}

func prepareProcessOutputRoot(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", nil
	}

	root, err := coagenthome.ProcessProjectDir(projectID)
	if err != nil {
		return "", fmt.Errorf("resolve process output root: %w", err)
	}

	// Mount the directory before any process starts so later output stays visible.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create process output root: %w", err)
	}

	return root, nil
}

// canonicalSourceRepoRoot resolves the main repository root a linked worktree
// coagent created inherits its sandbox rules from; empty when this session
// does not run in such a worktree.
func canonicalSourceRepoRoot(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", nil
	}

	canonical, err := sandboxpolicy.CanonicalProjectKey(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve source repository root: %w", err)
	}

	return canonical, nil
}

// worktreeGitDir is the main repository's .git directory a linked worktree
// shares, or empty when this session does not run in such a worktree.
func worktreeGitDir(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}

	return filepath.Join(repoRoot, ".git")
}
