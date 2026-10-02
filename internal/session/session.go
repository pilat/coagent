package session

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const (
	compactionThreshold = 80000
)

// Events receives live, non-durable notifications for the controller.
type Events interface {
	Emit(sessionevent.Notification)
}

// Service executes one activation over the durable session transcript.
type Service interface {
	Run(context.Context) (RunResult, error)
	PrepareUserMessage(string) (string, error)
	SetModel(string, string) error
	AgentTypes() *registry.Set
	RegisterGatedTool(tool.Tool) bool
	PendingExternalCalls() []PendingToolCall
	HasPendingWork() bool
	Close()
}

// ActiveSubagentInfo summarizes one of a session's in-flight children for the
// activation-start context snapshot. The daemon (owner of the subagent ledger)
// pushes these at session create/resume.
type ActiveSubagentInfo struct {
	ChildID  int64
	Blocking bool
	State    string
}

// ActiveProcessInfo summarizes one advertised process owned by this session.
type ActiveProcessInfo struct {
	ID         string
	OutputPath string
}

var _ Service = (*svc)(nil)

type svc struct {
	workDir           string
	gitClient         git.Client
	projectID         int64
	llmClient         llm.Client
	stack             *builtin.Stack
	loader            loader.Service
	todoStore         todo.Service
	registry          tool.Registry
	activationIndex   map[string]string
	currentActivation *tool.ActivationGrant
	agentsMD          string
	memoryStore       memory.CuratedStore
	store             sessionstore.RuntimeStore
	rootID            int64
	id                int64
	model             string
	agentType         registry.AgentType
	agentTypes        *registry.Set
	iterationOffset   int
	reasoningLevel    string
	// modelMu guards the mutable model triplet (llmClient/model/reasoningLevel),
	// swapped by handleSetModel (daemon goroutine) while the loop reads them.
	modelMu         sync.RWMutex
	closeOnce       sync.Once
	cfg             *config.Config
	stamper         timestamper
	prompt          *promptBuilder
	events          Events
	schedules       string
	outputEnabled   bool
	preserveStopped bool
	budgetFired     bool

	// Loop execution state
	ms           *messageStore
	loopDetector *loopDetector
	// resumeCompletion seeds the reply cache on resume; the loop re-reads
	// SQLite per turn, so a crash between resume and the next turn still
	// observes the durable state.
	resumeCompletion  *sessionstore.CompletionCheckState
	compactionFocus   string // optional /compact focus, set for one compact() then cleared
	pendingCompaction bool
	compactionInput   *PendingInput
	// Summary row of the last auto compaction; keys its success output
	// (`compaction:<id>:succeeded`) so a replay is an idempotent no-op.
	compactionSummaryDBID int64
	compactionOutputs     []*sessionstore.OutputCommit
	// compactionDeferAnnounced survives the session object: the daemon rebuilds
	// this svc on every wake, so per-run state would re-announce per wake.
	compactionDeferAnnounced bool
	suspended                bool
	// stagedCalls are tool_call ids the daemon has already started outside work
	// for (call id → tool name). Loop-read only; set once at construction.
	stagedCalls map[string]string
	// Reads the daemon's background ledgers live; nil outside a daemon.
	activeSubagents          []ActiveSubagentInfo
	activeProcesses          []ActiveProcessInfo
	activeBackgroundSnapshot string
	// Under modelMu with the model triplet: a measurement describes one model's
	// window and tokenizer. nil baseline = nothing measured.
	baseline   *contextBaseline
	modelEpoch uint64
}

type params struct {
	Config      *config.Config
	LLMClient   llm.Client
	TodoStore   todo.Service
	Loader      loader.Service
	Stack       *builtin.Stack
	Registry    tool.Registry
	Store       sessionstore.RuntimeStore
	GitClient   git.Client
	MemoryStore memory.CuratedStore
}

type options struct {
	ID             int64
	AgentType      registry.AgentType
	ProjectID      int64
	RootID         int64 // 0 = root session (rootID := ID); non-zero = child's root
	ReasoningLevel string

	// DB-based resume fields
	ResumeMessages  []llmwire.Message
	ResumeRowIDs    []int64
	ResumeIteration int
	ResumeTodoItems []*todo.Item
	LastActivityAt  time.Time
	Events          Events
	Schedules       string
	OutputEnabled   bool

	// ContextBaseline is the persisted provider measurement from the previous
	// run; nil when none was taken. Installed only when it describes this
	// session's model.
	ContextBaseline *sessionstore.ContextBaseline

	// PreserveStopped marks a command-only activation of a stopped root: the
	// run must not reactivate the root past its prior stopped status.
	PreserveStopped bool

	// ActiveSubagents is the daemon-pushed set of this session's in-flight
	// children, rendered into activation-start context.
	ActiveSubagents []ActiveSubagentInfo
	ActiveProcesses []ActiveProcessInfo

	// ExtraSkills are daemon-injected, session-scoped skills that are registered
	// for discovery and activated directly in the system prompt.
	ExtraSkills []*loader.Skill

	// ExternalCalls are tool_call ids the daemon owes a result for.
	ExternalCalls map[string]string

	// ResumeCompletionState is the durable check, reply obligation, and empty
	// streak the daemon hands back on resume. SQLite stays authoritative; the
	// loop re-reads it per turn, these fields only seed the in-memory caches.
	ResumeCompletionState *sessionstore.CompletionCheckState

	// CompactionDeferAnnounced is the previous run's deferral-notice verdict.
	CompactionDeferAnnounced bool
}

func newWithOptions(ctx context.Context, p params, opts options) (Service, error) {
	log := logger.Ctx(ctx).Named("session.new")

	workDir := p.Config.WorkDir
	if workDir == "" {
		workDir = "."
	}

	agentType := opts.AgentType
	if agentType == "" {
		agentType = registry.AgentTypeBuild
	}

	projectSubagents := loadProjectSubagents(ctx, p, workDir)
	set := registry.NewSet(projectSubagents)

	agentConfig, ok := set.Get(agentType)
	if !ok {
		return nil, fmt.Errorf("unknown agent type: %s", agentType)
	}

	var openingContext string
	if !agentConfig.OmitProjectContext {
		openingContext = loadProjectInstructions(ctx, p, workDir)
		if p.MemoryStore != nil && opts.ProjectID != 0 {
			openingContext += buildMemoriesSection(ctx, p.MemoryStore, opts.ProjectID)
		}

		// Injected before the prompt is built: a skill registered after the skills
		// section is rendered is one the model never learns exists.
		for _, skill := range opts.ExtraSkills {
			p.Loader.RegisterSkill(skill)
		}
	}

	session := newSession(p, opts, workDir, agentConfig, openingContext)
	session.agentTypes = set
	session.prompt = buildPrompt(p, opts, workDir, agentConfig)
	// The native-search bit must precede setupRegistry: the tools section it
	// builds reports search guidance for the active client.
	session.prompt.setNativeSearch(p.Config.UnifiedConfig.SearchNativeActive(p.Config.Model))
	session.setupRegistry(p, agentConfig)

	if err := session.applyResumeOrInit(ctx, opts, log); err != nil {
		return nil, err
	}

	session.activeBackgroundSnapshot = buildActiveBackgroundSection(
		opts.ActiveProcesses,
		opts.ActiveSubagents,
	)

	if opts.ReasoningLevel != "" {
		session.reasoningLevel = opts.ReasoningLevel
	}

	sessionID := strconv.FormatInt(session.id, 10)
	if session.id != session.rootID {
		sessionID = fmt.Sprintf("%d:%d", session.rootID, session.id)
	}

	session.llmClient.SetSessionID(sessionID)
	session.llmClient.SetReasoningLevel(session.reasoningLevel)

	log.Info(
		"agent_config",
		zap.Int("system_prompt_len", len(session.prompt.systemPrompt())),
		zap.Int("tools_count", len(session.registry.List())),
		zap.Int64("root_id", session.rootID),
	)

	return session, nil
}

// newSession constructs a bare svc with all fields populated except prompt and registry.
func newSession(p params, opts options, workDir string, agentConfig registry.AgentTypeConfig, agentsMD string) *svc {
	s := &svc{
		workDir:         workDir,
		projectID:       opts.ProjectID,
		llmClient:       p.LLMClient,
		stack:           p.Stack,
		loader:          p.Loader,
		todoStore:       p.TodoStore,
		registry:        p.Registry,
		memoryStore:     p.MemoryStore,
		agentsMD:        agentsMD,
		store:           p.Store,
		model:           p.Config.Model,
		agentType:       agentConfig.Name,
		reasoningLevel:  string(llm.ReasoningMedium),
		gitClient:       projectContextGitClient(p.GitClient, agentConfig),
		cfg:             p.Config,
		stamper:         timestamper{lastActivity: opts.LastActivityAt},
		loopDetector:    newLoopDetector(),
		stagedCalls:     opts.ExternalCalls,
		schedules:       opts.Schedules,
		events:          opts.Events,
		outputEnabled:   opts.OutputEnabled,
		preserveStopped: opts.PreserveStopped,

		compactionDeferAnnounced: opts.CompactionDeferAnnounced,
		resumeCompletion:         opts.ResumeCompletionState,
		activeSubagents:          opts.ActiveSubagents,
		activeProcesses:          opts.ActiveProcesses,
	}
	var msStore sessionstore.RuntimeStore

	if s.store != nil {
		msStore = s.store
	}

	s.ms = newMessageStore(msStore, opts.ID)
	s.attachImageAuthorizer(s.llmClient)
	s.confineGitClient()

	return s
}

func (s *svc) SetModel(model, reasoningLevel string) error {
	return s.handleSetModel(model, reasoningLevel)
}

func (s *svc) AgentTypes() *registry.Set {
	return s.agentTypes
}

func (s *svc) SkillCatalog() loader.SkillCatalog {
	return s.loader
}

// RegisterGatedTool applies the same agent-type filter used at construction
// (filterRegistryForAgent) to a single tool registered after the fact.
func (s *svc) RegisterGatedTool(t tool.Tool) bool {
	if len(s.agentTypes.FilterTools([]string{t.ID()}, s.agentType)) == 0 {
		return false
	}

	s.registry.Register(t)

	return true
}

func (s *svc) Close() {
	s.closeOnce.Do(func() {
		if s.llmClient != nil {
			if err := s.closeLLM(); err != nil {
				logger.Named("session.close").Warn("llm_close_failed", zap.Error(err))
			}
		}

		if s.stack != nil {
			_ = s.stack.Close()
		}
	})
}

// RequestCompaction requests a forced compaction at the next loop iteration.
func (s *svc) RequestCompaction() {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.pendingCompaction = true
}

// buildPrompt assembles the promptBuilder for a session before tool registration.
// Registry-derived sections stay empty until refreshRegistrySections runs.
func buildPrompt(
	p params,
	opts options,
	workDir string,
	agentConfig registry.AgentTypeConfig,
) *promptBuilder {
	basePrompt := agentConfig.Prompt +
		fmt.Sprintf(
			"\n\n# Environment\n- Working directory: %s\n- Platform: %s/%s\n- Timestamped user input is prefixed with `[+elapsed DOW YYYY-MM-DD HH:MM ZONE ±HH:MM]`, where `+elapsed` is optional. Use it for temporal reasoning.",
			workDir,
			runtime.GOOS,
			runtime.GOARCH,
		)

	var modelsSection string
	if !agentConfig.OmitProjectContext {
		modelsSection = buildModelsSection(p.Config.Model)
	}

	return newPromptBuilder(
		basePrompt,
		modelsSection,
		activeProjectSkills(opts.ExtraSkills, agentConfig)...,
	)
}

func projectContextGitClient(client git.Client, agentConfig registry.AgentTypeConfig) git.Client {
	if agentConfig.OmitProjectContext {
		return nil
	}

	return client
}

func activeProjectSkills(skills []*loader.Skill, agentConfig registry.AgentTypeConfig) []*loader.Skill {
	if agentConfig.OmitProjectContext {
		return nil
	}

	return skills
}

// filterRegistryForAgent creates a filtered copy of the registry based on agent
// type config. Todo-tool exclusions live in the agent config's Tools list (the
// set normalizes them for subagents), so no agent-mode special-case is needed here.
func filterRegistryForAgent(set *registry.Set, reg tool.Registry, agentConfig registry.AgentTypeConfig) tool.Registry {
	allIDs := reg.IDs()
	allowedIDs := set.FilterTools(allIDs, agentConfig.Name)

	return reg.Filter(allowedIDs)
}

// unresolvedToolCalls returns id→name for tool_calls in the current (most recent)
// assistant turn that have no matching tool_result. Returns nil when that turn is
// text-only, or when a newer user message has superseded it — a tool call left
// dangling before a user interruption is abandoned, not pending (repair still
// stubs it for API validity, independently of this scan).
func unresolvedToolCalls(messages []llmwire.Message) map[string]string {
	for i, v := range slices.Backward(messages) {
		if v.Role == llmwire.RoleUser {
			return nil
		}

		if v.Role != llmwire.RoleAssistant {
			continue
		}

		if len(v.ToolCalls) == 0 {
			return nil
		}

		resolved := make(map[string]bool)

		for j := i + 1; j < len(messages); j++ {
			if messages[j].Role == llmwire.RoleTool {
				resolved[messages[j].ToolCallID] = true
			}
		}

		out := make(map[string]string)

		for _, tc := range v.ToolCalls {
			if !resolved[tc.ID] {
				out[tc.ID] = tc.Name
			}
		}

		return out
	}

	return nil
}

// setupRegistry filters the tool registry for the agent type, registers session-scoped tools,
// and finalises the dynamic tools section in the prompt.
func (s *svc) setupRegistry(p params, agentConfig registry.AgentTypeConfig) {
	// Bind the skill tool to this session's loader, overriding whatever the
	// incoming registry carried.
	p.Registry.Register(builtin.NewSkillTool(p.Loader))

	filtered := filterRegistryForAgent(s.agentTypes, p.Registry, agentConfig)
	s.registry = filtered
	registerSessionTools(s)
	s.refreshRegistrySections()
}

// refreshRegistrySections recomputes the prompt sections derived from the live tool
// registry, which the daemon extends after construction. Once per activation.
func (s *svc) refreshRegistrySections() {
	s.prompt.refreshToolsSection(s.registry)

	var skills string
	if s.registry.Get(tool.IDSkill) != nil {
		skills = buildSkillsSection(s.loader)
	}

	s.prompt.setSkillsSection(skills)

	// A section naming a tool the allowlist removed is worse than no section.
	var subagents string
	if s.registry.Get(tool.IDTask) != nil {
		subagents = buildSubagentsSection(s.loader)
	}

	s.prompt.setSubagentsSection(subagents)
}

// compactionRequested reports whether a forced compaction is queued, without
// consuming it.
func (s *svc) compactionRequested() bool {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	return s.pendingCompaction
}

func (s *svc) compactionCommandInput() *PendingInput {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	if s.compactionInput == nil {
		return nil
	}

	input := *s.compactionInput

	return &input
}

func (s *svc) setCompactionCommandInput(input PendingInput) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionInput = &input
}

func (s *svc) clearCompactionCommandInput() {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionInput = nil
}

// setCompactionFocus records (or clears) the one-shot /compact focus.
func (s *svc) setCompactionFocus(focus string) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionFocus = focus
}

// consumePendingCompaction atomically reads and clears the pending compaction request.
func (s *svc) consumePendingCompaction() bool {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	pending := s.pendingCompaction
	s.pendingCompaction = false

	return pending
}

func (s *svc) contextWindow() int {
	if cw := s.currentLLM().ContextWindow(); cw > 0 {
		return cw
	}

	return compactionThreshold
}

// seedResumeCompletion installs the durable completion projection into the
// in-memory reply cache. The loop still re-reads SQLite per turn, so this is
// a seed, not authority.
func (s *svc) seedResumeCompletion(state *sessionstore.CompletionCheckState) {
	s.resumeCompletion = state
}

// applyResumeOrInit sets session IDs and either restores state from DB or persists the initial state.
func (s *svc) applyResumeOrInit(ctx context.Context, opts options, log *zap.Logger) error {
	if opts.ID == 0 {
		return errors.New("session ID is required")
	}

	s.id = opts.ID
	if opts.RootID != 0 {
		s.rootID = opts.RootID
	} else {
		s.rootID = opts.ID
	}

	if opts.ResumeMessages != nil {
		if opts.ResumeRowIDs == nil {
			s.ms.setMessages(opts.ResumeMessages)
		} else if err := s.ms.setMessagesWithRowIDs(opts.ResumeMessages, opts.ResumeRowIDs); err != nil {
			return fmt.Errorf("restore transcript identities: %w", err)
		}

		s.iterationOffset = opts.ResumeIteration

		if len(opts.ResumeTodoItems) > 0 {
			s.todoStore.Replace(opts.ResumeTodoItems)
		}

		s.seedResumeCompletion(opts.ResumeCompletionState)

		s.installPersistedBaseline(opts.ContextBaseline)

		log.Info("resumed_from_db", zap.Int64("root_id", s.rootID), zap.Int("iteration", opts.ResumeIteration))

		return nil
	}

	if err := s.persistState(ctx, 0, "active"); err != nil {
		return fmt.Errorf("persist initial state: %w", err)
	}

	return nil
}

// attachImageAuthorizer gives a client the session's current filesystem
// authority, so a deferred attachment read is re-authorized at materialization
// and a revoked grant is not re-read through a historical reference.
func (s *svc) attachImageAuthorizer(client llm.Client) {
	if client == nil || s.stack == nil {
		return
	}

	client.SetImageAuthorizer(s.stack.Access())
}

// confineGitClient routes session-owned Git through the session's confinement
// runner, so project-dependent hooks, helpers and fsmonitor cannot execute
// outside the policy. A session without a runner keeps the daemon client.
func (s *svc) confineGitClient() {
	if s.gitClient == nil || s.stack == nil {
		return
	}

	if sandboxed := s.stack.SandboxedGit(); sandboxed != nil {
		s.gitClient = sandboxed
	}
}
