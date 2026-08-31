package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/shellenv"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Factory creates isolated session instances for different workdirs.
type Factory interface {
	Create(ctx context.Context, opts CreateOptions) (Service, error)
}

// CreateOptions configures a new or resumed session.
type CreateOptions struct {
	ID             int64
	WorkDir        string
	Model          string
	ProjectID      int64
	AgentType      string // "" = root build agent; set for subagent sessions
	RootID         int64
	ReasoningLevel string
	Iteration      int
	TodoItems      string
	LastActivityAt time.Time
	InputBoundary  InputBoundary
	OutputEnabled  bool
	BudgetGate     BudgetGate

	// SettlementOpen marks a lifecycle settlement open: the initial state is not
	// persisted, so a stopping root is never reactivated by /stop settlement.
	SettlementOpen bool

	// PreserveStoppedStatus marks a command-only activation of a stopped root:
	// read-only boundary commands run, but the run must not reactivate the root
	// past its prior stopped status.
	PreserveStoppedStatus bool

	// ActiveSubagents is the daemon-pushed set of this session's in-flight
	// children, rendered into the pinned "# Active subagents" prompt section.
	ActiveSubagents []ActiveSubagentInfo

	// ActiveSubagentsProvider reads the same ledger live, for the section a
	// compaction summary carries. Nil outside a daemon.
	ActiveSubagentsProvider func(context.Context) []ActiveSubagentInfo

	// ExtraSkills are session-scoped instructions the daemon registers and
	// activates in the system prompt without waiting for a model tool call.
	ExtraSkills []*loader.Skill

	// StagedExternalCalls are call ids the daemon owes a result for: neither
	// re-executed nor advanced past, until the matching injection arrives.
	StagedExternalCalls map[string]string

	// ContextBaseline carries the persisted provider measurement back in on
	// resume; nil when the previous run never measured.
	ContextBaseline *sessionstore.ContextBaseline

	// CompactionDeferAnnounced carries the previous run's verdict back in: the
	// human is told once that a queued /compact is waiting, not once per wake.
	CompactionDeferAnnounced bool
}

var _ Factory = (*factory)(nil)

// FactoryOption customizes a factory (test seams).
type FactoryOption func(*factory)

// WithLLMClientFactory overrides how per-session LLM clients are constructed.
// Used by tests to inject a scripted fake LLM.
func WithLLMClientFactory(fn func(cfg *config.Config) (llm.Client, error)) FactoryOption {
	return func(f *factory) { f.newLLMClient = fn }
}

type factory struct {
	cfg              *config.Config
	secrets          config.Secrets
	memoryStore      memory.CuratedStore
	store            sessionstore.RuntimeStore
	gitClient        git.Client
	mcpPool          mcp.Pool
	mcpStore         mcpstore.Store
	marketplaceCache loader.MarketplaceCache
	provider         shellenv.Provider
	newLLMClient     func(cfg *config.Config) (llm.Client, error)
}

// NewFactory creates a session factory with shared dependencies.
func NewFactory(
	cfg *config.Config,
	secrets config.Secrets,
	memoryStore memory.CuratedStore,
	store sessionstore.RuntimeStore,
	gitClient git.Client,
	mcpPool mcp.Pool,
	mcpStore mcpstore.Store,
	marketplaceCache loader.MarketplaceCache,
	provider shellenv.Provider,
) Factory {
	return NewFactoryWithOptions(
		cfg, secrets, memoryStore, store, gitClient, mcpPool, mcpStore, marketplaceCache, provider,
	)
}

// NewFactoryWithOptions is NewFactory with customization hooks (test seams).
func NewFactoryWithOptions(
	cfg *config.Config,
	secrets config.Secrets,
	memoryStore memory.CuratedStore,
	store sessionstore.RuntimeStore,
	gitClient git.Client,
	mcpPool mcp.Pool,
	mcpStore mcpstore.Store,
	marketplaceCache loader.MarketplaceCache,
	provider shellenv.Provider,
	opts ...FactoryOption,
) Factory {
	f := &factory{
		cfg:              cfg,
		secrets:          secrets,
		memoryStore:      memoryStore,
		store:            store,
		gitClient:        gitClient,
		mcpPool:          mcpPool,
		mcpStore:         mcpStore,
		marketplaceCache: marketplaceCache,
		provider:         provider,
		newLLMClient:     llm.NewClient,
	}

	for _, o := range opts {
		o(f)
	}

	return f
}

// Create produces an isolated session for the given workdir.
func (f *factory) Create(ctx context.Context, opts CreateOptions) (Service, error) {
	if opts.ID == 0 {
		return nil, errors.New("session ID is required")
	}

	if opts.WorkDir == "" {
		return nil, errors.New("workdir is required")
	}

	cfg := f.sessionConfig(opts.WorkDir, opts.Model)

	llmClient, err := f.newLLMClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create LLM client: %w", err)
	}
	// Until Create returns a Service, the factory owns the client. Every failure
	// below must release it; after success, Service.Close takes over ownership.
	owned := true
	defer func() {
		if owned {
			_ = llmClient.Close()
		}
	}()

	// Try loading existing messages from DB (resume path).
	ms := newMessageStore(f.store, opts.ID)
	if f.store != nil {
		if err := ms.reloadMessages(ctx); err != nil {
			return nil, fmt.Errorf("load messages: %w", err)
		}
	}

	sess, err := f.build(ctx, cfg, llmClient, opts, ms.getMessages())
	if err != nil {
		return nil, err
	}

	owned = false

	return sess, nil
}

// sessionConfig creates a per-session config clone with the given workdir and model.
func (f *factory) sessionConfig(workDir, model string) *config.Config {
	cfg := *f.cfg
	cfg.WorkDir = workDir

	if model != "" {
		cfg.Model = model
	}

	if cfg.Model == "" {
		cfg.Model = cfg.DefaultModel()
	}

	return &cfg
}

// build assembles all isolated per-session dependencies and delegates
// to newWithOptions.
func (f *factory) build(
	ctx context.Context,
	cfg *config.Config,
	llmClient llm.Client,
	opts CreateOptions,
	messages []llmwire.Message,
) (Service, error) {
	// Validate todo items before acquiring any resources, so a bad payload
	// can't leak the stack's LSP/MCP handles on an early return.
	var todoItems []*todo.Item

	if opts.TodoItems != "" && opts.TodoItems != "[]" {
		if err := json.Unmarshal([]byte(opts.TodoItems), &todoItems); err != nil {
			return nil, fmt.Errorf("unmarshal todo items: %w", err)
		}
	}

	todoSvc := todo.New()
	ldr := loader.New(f.marketplaceCache)

	reg, stack, err := f.buildRegistry(ctx, cfg, ldr, todoSvc, opts.ProjectID, opts.ID)
	if err != nil {
		return nil, err
	}

	var resumeMessages []llmwire.Message
	if len(messages) > 0 {
		resumeMessages = messages
	}

	p := params{
		Config:      cfg,
		LLMClient:   llmClient,
		TodoStore:   todoSvc,
		Loader:      ldr,
		Stack:       stack,
		Registry:    reg,
		Store:       f.store,
		GitClient:   f.gitClient,
		MemoryStore: f.memoryStore,
	}

	sessOpts := options{
		ID:              opts.ID,
		AgentType:       registry.AgentType(opts.AgentType),
		ProjectID:       opts.ProjectID,
		RootID:          opts.RootID,
		ReasoningLevel:  opts.ReasoningLevel,
		ResumeMessages:  resumeMessages,
		ResumeIteration: opts.Iteration,
		ResumeTodoItems: todoItems,
		LastActivityAt:  opts.LastActivityAt,
		InputBoundary:   opts.InputBoundary,
		OutputEnabled:   opts.OutputEnabled,
		BudgetGate:      opts.BudgetGate,
		SettlementOpen:  opts.SettlementOpen,
		PreserveStopped: opts.PreserveStoppedStatus,
		ActiveSubagents: opts.ActiveSubagents,
		ContextBaseline: opts.ContextBaseline,

		ActiveSubagentsProvider:  opts.ActiveSubagentsProvider,
		ExtraSkills:              opts.ExtraSkills,
		StagedExternalCalls:      opts.StagedExternalCalls,
		CompactionDeferAnnounced: opts.CompactionDeferAnnounced,
	}

	sess, err := newWithOptions(ctx, p, sessOpts)
	if err != nil {
		if stack != nil {
			_ = stack.Close()
		}

		return nil, err
	}

	return sess, nil
}

// buildRegistry assembles the session's core-tools + MCP stack. The returned
// stack MUST be Closed by the caller.
func (f *factory) buildRegistry(
	ctx context.Context,
	cfg *config.Config,
	ldr loader.Service,
	todoSvc todo.Service,
	projectID, sessionID int64,
) (tool.Registry, *builtin.Stack, error) {
	stack, err := builtin.BuildStack(ctx, builtin.StackConfig{
		WorkDir:         cfg.WorkDir,
		Pool:            f.mcpPool,
		Servers:         resolveMCPServers(ctx, f.mcpStore, f.secrets, projectID),
		Unified:         cfg.UnifiedConfig,
		Loader:          ldr,
		Todo:            todoSvc,
		TodoReplacement: &todoReplacement{store: f.store, sessionID: sessionID, memory: todoSvc},
		Provider:        f.provider,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build tool stack: %w", err)
	}

	return stack.Registry, stack, nil
}
