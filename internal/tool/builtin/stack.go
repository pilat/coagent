package builtin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

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
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// NetworkLease keeps one tree's network generation available to a tool stack.
type NetworkLease interface {
	Link() *bashsandbox.NetworkLink
	BindRunner(procexec.Runner, string)
	DialContext(context.Context, string, string) (net.Conn, error)
	Release()
}

// NetworkOwner acquires the generation for a root tree and effective policy.
type NetworkOwner interface {
	Acquire(context.Context, int64, sandboxpolicy.Policy) (NetworkLease, error)
}

// StackConfig configures a session-scoped local tool stack.
type StackConfig struct {
	ProjectID         int64
	SessionID         int64
	RootSessionID     int64 // 0 = this session is its own root
	WorkDir           string
	RepoRoot          string                      // main repository path for worktree sessions; empty otherwise
	CreatedWorktree   bool                        // controller-created /gwt provenance for escalation inheritance
	Servers           map[string]mcp.ServerConfig // resolved MCP definitions; empty = no MCP
	Unified           *config.UnifiedConfig       // for the sandbox config
	Loader            loader.Service
	Todo              todo.Service
	TodoReplacement   TodoReplacement
	FileReadTracker   FileReadTracker
	Provider          shellenv.Provider // optional injected provider; nil creates a stack-owned provider
	ShieldsUp         bool
	ProcessService    backgroundprocess.Service // session-bound process lifecycle; may be nil
	OutputDirOverride string                    // test-only output root override; empty = default
	NetworkOwner      NetworkOwner
}

// Stack is a session-scoped local tool set. It owns the LSP manager and MCP access
// it creates; callers MUST Close() it on every exit path.
type Stack struct {
	Registry    tool.Registry
	lspMgr      lsp.Manager
	mcpSvc      mcp.Service // nil when no MCP servers configured
	provider    shellenv.Provider
	ownProvider bool
	runner      bashsandbox.Runner
	access      safefile.Access
	network     NetworkLease
	sessionID   int64
	web         []*http.Transport
}

// BuildStack assembles the local tool registry: core builtins plus MCP tools.
//
//nolint:funlen,wsl_v5 // All tools share one runner, rooted access and ordered cleanup.
func BuildStack(ctx context.Context, cfg StackConfig) (*Stack, error) {
	canonicalRoot, err := canonicalProjectRoot(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}

	sandboxCfg, err := bashSandboxConfig(ctx, cfg, canonicalRoot)
	if err != nil {
		return nil, fmt.Errorf("compile sandbox policy: %w", err)
	}
	provider, ownProvider := stackProvider(cfg)
	closeProvider := func() {
		if ownProvider {
			_ = provider.Close()
		}
	}
	var network NetworkLease
	if sandboxCfg.Enabled {
		network, err = acquireStackNetwork(ctx, cfg, &sandboxCfg)
		if err != nil {
			closeProvider()
			return nil, err
		}
	}

	//nolint:contextcheck // Sandbox preflight is a bounded process-wide self-test.
	bashRunner, err := bashsandbox.New(sandboxCfg, provider)
	if err != nil {
		if network != nil {
			network.Release()
		}
		closeProvider()
		return nil, fmt.Errorf("create bash sandbox: %w", err)
	}
	if network != nil {
		network.BindRunner(bashRunner, cfg.WorkDir)
	}

	// The runner materializes the declared read-write grants, so file access can
	// hold every present one. Both surfaces therefore share one compiled policy.
	access, err := safefile.New(sandboxCfg.Policy, cfg.WorkDir)
	if err != nil {
		if network != nil {
			network.Release()
		}
		closeProvider()
		return nil, fmt.Errorf("create filesystem access: %w", err)
	}

	mutator, err := newFileMutator(sandboxCfg.Enabled, bashRunner)
	if err != nil {
		_ = access.Close()
		if network != nil {
			network.Release()
		}
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
			if network != nil {
				network.Release()
			}
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
		network,
	)

	// MCP failure degrades to a builtin-only stack: a broken MCP server must not block sessions.
	mcpSvc, err := mcp.AcquireForWorkDir(ctx, cfg.Servers, cfg.WorkDir, provider, processRunner)
	if err != nil {
		logger.Ctx(ctx).Warn("mcp_acquire_failed", zap.Error(err))
	}

	if mcpSvc != nil {
		mcpSvc.RegisterTools(registry)
	}

	logger.Ctx(ctx).Named("tool.stack").Info("stack_started",
		zap.Int64("session_id", cfg.SessionID), zap.Int64("root_id", rootSessionID(cfg)),
		zap.Bool("sandbox", sandboxCfg.Enabled), zap.Bool("network", network != nil),
		zap.Bool("shields_up", cfg.ShieldsUp))

	return &Stack{
		Registry: registry, lspMgr: lspMgr, mcpSvc: mcpSvc, runner: bashRunner, access: access,
		provider: provider, ownProvider: ownProvider, network: network, web: webTransports,
		sessionID: cfg.SessionID,
	}, nil
}

func stackProvider(cfg StackConfig) (shellenv.Provider, bool) {
	if cfg.ShieldsUp {
		return nil, false
	}

	if cfg.Provider != nil {
		return cfg.Provider, false
	}

	return shellenv.New(), true
}

func acquireStackNetwork(ctx context.Context, cfg StackConfig, sandboxCfg *bashsandbox.Config) (NetworkLease, error) {
	if cfg.NetworkOwner == nil {
		return nil, errors.New("sandbox network owner is unavailable")
	}

	network, err := cfg.NetworkOwner.Acquire(ctx, rootSessionID(cfg), sandboxCfg.Policy)
	if err != nil {
		return nil, fmt.Errorf("acquire sandbox network: %w", err)
	}

	sandboxCfg.Network = network.Link()

	return network, nil
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

// Close releases the LSP manager and MCP access the stack owns.
//
//nolint:wsl_v5 // Close preserves subsystem order while joining independent failures.
func (s *Stack) Close() error {
	logger.Named("tool.stack").Info("stack_stopped",
		zap.Int64("session_id", s.sessionID), zap.Bool("network_released", s.network != nil))

	for _, transport := range s.web {
		transport.CloseIdleConnections()
	}
	if s.network != nil {
		defer s.network.Release()
	}
	if s.lspMgr != nil {
		s.lspMgr.Close()
	}

	if s.mcpSvc != nil {
		s.mcpSvc.Stop()
	}
	var err error
	if s.access != nil {
		err = errors.Join(err, s.access.Close())
	}
	if s.ownProvider {
		err = errors.Join(err, s.provider.Close())
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
	leases ...NetworkLease,
) []*http.Transport {
	var network NetworkLease
	if len(leases) > 0 {
		network = leases[0]
	}

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

	fetch := newWebFetchToolWithNetwork(network)
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
	if searchTool, transport := newSearchToolFromConfig(unified, network); searchTool != nil {
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
func newSearchToolFromConfig(unified *config.UnifiedConfig, leases ...NetworkLease) (tool.Tool, *http.Transport) {
	if unified == nil {
		return nil, nil
	}

	s := unified.Tools.Search
	if !s.SearchActive() {
		return nil, nil
	}

	var network NetworkLease
	if len(leases) > 0 {
		network = leases[0]
	}

	transport := webTransport(network)
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
// configuration. Escalation comes only from the operator's config: global and
// project grants are unioned, and raised shields remove every profile entry.
func bashSandboxConfig(ctx context.Context, cfg StackConfig, canonicalRoot string) (bashsandbox.Config, error) {
	sessionKey := fmt.Sprintf("session:%d", cfg.SessionID)

	if cfg.Unified == nil || !cfg.Unified.Sandbox.Enabled {
		return bashsandbox.Config{WorkDir: cfg.WorkDir, SessionKey: sessionKey}, nil
	}

	catalog, err := cfg.Unified.SandboxCatalog()
	if err != nil {
		return bashsandbox.Config{}, fmt.Errorf("load sandbox catalog: %w", err)
	}

	substrate, err := bashsandbox.ExecutionSubstrate()
	if err != nil {
		return bashsandbox.Config{}, fmt.Errorf("probe execution substrate: %w", err)
	}

	tempRoot, err := projectTempRoot(cfg.ProjectID, canonicalRoot)
	if err != nil {
		return bashsandbox.Config{}, err
	}

	sourceRoot := verifiedWorktreeSource(
		ctx, canonicalRoot, cfg.RepoRoot, projectpath.ResolveWorktreesRoot(cfg.Unified), cfg.CreatedWorktree,
	)

	escalated, err := projectEscalation(cfg.Unified.Sandbox.Projects, canonicalRoot, sourceRoot)
	if err != nil {
		return bashsandbox.Config{}, err
	}

	outputRoots, err := processOutputRoots(cfg)
	if err != nil {
		return bashsandbox.Config{}, err
	}

	compiled, err := sandboxpolicy.Compile(catalog, sandboxpolicy.Request{
		ProjectRoot:        canonicalRoot,
		ProjectID:          cfg.ProjectID,
		WorkDir:            cfg.WorkDir,
		TempRoot:           tempRoot,
		Shields:            cfg.ShieldsUp,
		GlobalEscalated:    cfg.Unified.Sandbox.Escalated,
		ProjectEscalated:   escalated,
		LegacyWritable:     cfg.Unified.Sandbox.WritablePaths,
		Environment:        config.SandboxEnvironment(),
		GitMetadataRoot:    worktreeGitRoot(cfg.RepoRoot),
		Substrate:          substrate,
		ProcessOutputRoots: outputRoots,
	})
	if err != nil {
		return bashsandbox.Config{}, fmt.Errorf("compile sandbox policy: %w", err)
	}

	return bashsandbox.Config{
		Enabled: true, Shields: cfg.ShieldsUp, Policy: compiled,
		WorkDir: cfg.WorkDir, SessionKey: sessionKey,
	}, nil
}

// processOutputRoots names the recorded process output this session tree may
// read: its own directory and its tree root's, never a sibling's.
func processOutputRoots(cfg StackConfig) ([]string, error) {
	if cfg.ProjectID <= 0 {
		return nil, nil
	}

	base, err := coagenthome.ProcessProjectDir(cfg.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("resolve process output dir: %w", err)
	}

	roots := []string{filepath.Join(base, strconv.FormatInt(cfg.SessionID, 10))}

	if root := rootSessionID(cfg); root > 0 && root != cfg.SessionID {
		roots = append(roots, filepath.Join(base, strconv.FormatInt(root, 10)))
	}

	return roots, nil
}

// projectTempRoot resolves the private temporary backing for one project: a
// durable project id when there is one, a canonical-root identity otherwise.
func projectTempRoot(projectID int64, canonicalRoot string) (string, error) {
	identity := coagenthome.SandboxPathIdentity(canonicalRoot)

	if projectID > 0 {
		durable, err := coagenthome.SandboxProjectIdentity(projectID)
		if err != nil {
			return "", fmt.Errorf("resolve project identity: %w", err)
		}

		identity = durable
	}

	tempDir, err := coagenthome.SandboxTempDir(identity)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox temp dir: %w", err)
	}

	return tempDir, nil
}

// The origin marker grants provenance; Git and namespace checks reject stale
// or moved worktrees before selecting credential grants.
func verifiedWorktreeSource(ctx context.Context, workDir, repoRoot, worktreesRoot string, createdWorktree bool) string {
	if repoRoot == "" || !createdWorktree {
		return ""
	}

	resolvedRoot, err := filepath.EvalSymlinks(worktreesRoot)
	if err != nil || filepath.Dir(workDir) != filepath.Join(resolvedRoot, projectpath.RepoNamespace(repoRoot)) {
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	actualRoot, err := git.NewWorktreeClient().RepoRoot(ctx, workDir)
	if err != nil {
		return ""
	}

	actual, err := sandboxpolicy.CanonicalProjectKey(actualRoot)
	if err != nil || actual == workDir {
		return ""
	}

	configured, err := sandboxpolicy.CanonicalProjectKey(repoRoot)
	if err != nil || actual != configured {
		return ""
	}

	return actual
}

func projectEscalation(
	projects map[string]sandboxpolicy.ProjectOverride,
	canonicalRoot, repoRoot string,
) ([]string, error) {
	inheritedRoot := ""

	if repoRoot != "" {
		var err error

		inheritedRoot, err = sandboxpolicy.CanonicalProjectKey(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree repository %q: %w", repoRoot, err)
		}
	}

	var escalated []string

	for key, override := range projects {
		resolved, err := sandboxpolicy.CanonicalProjectKey(key)
		if err != nil {
			return nil, fmt.Errorf("resolve project key %q: %w", key, err)
		}

		if resolved == canonicalRoot || resolved == inheritedRoot {
			escalated = append(escalated, override.Escalated...)
		}
	}

	slices.Sort(escalated)
	escalated = slices.Compact(escalated)

	return escalated, nil
}

// worktreeGitRoot is the main repository's Git metadata a linked worktree
// shares. It stays writable in both shield states.
func worktreeGitRoot(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}

	return filepath.Join(repoRoot, ".git")
}
