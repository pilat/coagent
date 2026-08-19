package session

import (
	"context"
	"encoding/json"
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
	"github.com/pilat/coagent/internal/id"
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
	subagentEventTool   = "subagent_event"
)

// Service manages the state for a single agent session.
type Service interface {
	// RunDaemon runs the session with durable boundary input and notifications.
	RunDaemon(
		ctx context.Context,
		notify func(sessionevent.Notification),
	) (RunResult, error)
	PrepareUserMessage(message string) (string, error)

	// SetModel switches the LLM model and reasoning level for a running session.
	SetModel(model, reasoningLevel string) error

	// AgentTypes returns the session's immutable agent-type set (built-ins overlaid
	// with this session's project-local subagents).
	AgentTypes() *registry.Set

	// RegisterGatedTool registers t on the live registry if the session's own
	// agent-type allowlist permits it, and reports whether it was registered.
	RegisterGatedTool(t tool.Tool) bool

	// PendingExternalCalls returns every unresolved call whose result is produced
	// outside the ReAct loop. It scans the whole active transcript deliberately:
	// a newer synthetic event must never make an older suspended call disappear.
	PendingExternalCalls() []PendingToolCall

	// ResolvePendingCall durably answers one exact external call. Re-delivery is
	// idempotent; an unknown call or a mismatched tool name is rejected.
	ResolvePendingCall(ctx context.Context, call PendingToolCall, content string) (CallResolution, error)

	// InjectToolNotificationOnce adds a synthetic tool_call + tool_result pair,
	// applying one externally identified event at most once, including across
	// process restart and producer acknowledgement failure.
	InjectToolNotificationOnce(ctx context.Context, deliveryID, toolName, content string) (bool, error)

	// ResetContextAndInjectOnce discards the session's accumulated context
	// (transcript, compaction brief, todos) and starts a fresh turn with prompt as
	// the new task, at most once per delivery identity. Used by fresh schedules so
	// each tick runs from a blank slate. On error the previous transcript is left
	// intact.
	ResetContextAndInjectOnce(ctx context.Context, deliveryID, prompt string) (bool, error)

	// AppendDeliveredCompletion appends already-persisted subagent-completion
	// messages (built by the typed blocking/background builders, committed by the store's
	// DeliverCompletionAtomic) to the live in-memory transcript, stamping each with
	// its DB id. It does NOT write the DB. This synchronous refresh is mandatory on
	// the blocking path so the resumed loop observes the resolved task tool_use
	// before its next LLM call (otherwise it re-suspends and hangs).
	AppendDeliveredCompletion(msgs []llmwire.Message, msgIDs []int64)

	// HasPendingWork reports whether the last assistant turn has unresolved
	// tool calls that are not external-pending (sleep/task) — i.e. work the loop
	// should continue when resumed without a new stimulus.
	HasPendingWork() bool

	// Close releases session resources.
	Close()
}

// ActiveSubagentInfo summarizes one of a session's in-flight children for the
// pinned "# Active subagents" prompt section. The daemon (owner of the subagent
// ledger) pushes these at session create/resume.
type ActiveSubagentInfo struct {
	ChildID  int64
	Blocking bool
	State    string
}

var _ Service = (*svc)(nil)

type svc struct {
	workDir         string
	projectID       int64
	llmClient       llm.Client
	stack           *builtin.Stack
	loader          loader.Service
	todoStore       todo.Service
	registry        tool.Registry
	agentsMD        string
	memoryStore     memory.CuratedStore
	store           sessionstore.RuntimeStore
	rootID          int64
	id              int64
	model           string
	agentType       registry.AgentType
	agentTypes      *registry.Set
	iterationOffset int
	newLLMWithModel func(cfg *config.Config, model string) (llm.Client, error)
	reasoningLevel  string
	// modelMu guards the mutable model triplet (llmClient/model/reasoningLevel),
	// swapped by handleSetModel (daemon goroutine) while the loop reads them.
	modelMu   sync.RWMutex
	closeOnce sync.Once
	cfg       *config.Config
	loopOpts  loopOptions
	stamper   timestamper
	prompt    *promptBuilder
	boundary  InputBoundary

	// Loop execution state
	ms                *messageStore
	loopDetector      *loopDetector
	compactionBrief   string
	compactionFocus   string // optional /compact focus, set for one compact() then cleared
	pendingCompaction *int
	// compactionDeferAnnounced survives the session object: the daemon rebuilds
	// this svc on every wake, so per-run state would re-announce per wake.
	compactionDeferAnnounced bool
	suspended                bool
	maxIterations            int
	// stagedCalls are tool_call ids the daemon has already started outside work
	// for (call id → tool name). Loop-read only; set once at construction.
	stagedCalls map[string]string
	// Reads the daemon's subagent-link ledger live; nil outside a daemon.
	activeSubagentsProvider func(context.Context) []ActiveSubagentInfo
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
	ResumeIteration int
	ResumeTodoItems []*todo.Item
	CompactionBrief string
	LastActivityAt  time.Time
	InputBoundary   InputBoundary

	// ActiveSubagents is the daemon-pushed set of this session's in-flight
	// children, rendered into the pinned "# Active subagents" prompt section.
	ActiveSubagents []ActiveSubagentInfo

	// ActiveSubagentsProvider reads the same ledger live, at the moment a
	// compaction writes its summary — the create-time snapshot is stale by then.
	ActiveSubagentsProvider func(context.Context) []ActiveSubagentInfo

	// ExtraSkills are daemon-injected, session-scoped skills that are registered
	// for discovery and activated directly in the system prompt.
	ExtraSkills []*loader.Skill

	// StagedExternalCalls are tool_call ids the daemon owes a result for.
	StagedExternalCalls map[string]string

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

	// Setup runs before type resolution: it loads project-local subagents, which
	// seed the immutable Set the session's own agent type resolves against.
	agentsMD, projectSubagents := loadProjectContext(ctx, p, workDir)

	// Injected before the prompt is built: a skill registered after the skills
	// section is rendered is one the model never learns exists.
	for _, skill := range opts.ExtraSkills {
		p.Loader.RegisterSkill(skill)
	}

	set := registry.NewSet(projectSubagents)

	agentConfig, ok := set.Get(agentType)
	if !ok {
		return nil, fmt.Errorf("unknown agent type: %s", agentType)
	}

	session := newSession(p, opts, workDir, agentConfig, agentsMD)
	session.agentTypes = set
	session.prompt = buildPrompt(ctx, p, opts, workDir, agentConfig)
	session.setupRegistry(p, agentConfig)

	if err := session.applyResumeOrInit(ctx, opts, log); err != nil {
		return nil, err
	}

	session.prompt.setActiveSubagentsSection(buildActiveSubagentsSection(opts.ActiveSubagents))

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
		newLLMWithModel: llm.NewClientWithModel,
		cfg:             p.Config,
		stamper:         timestamper{lastActivity: opts.LastActivityAt},
		loopDetector:    newLoopDetector(),
		maxIterations:   agentConfig.MaxIterations,
		stagedCalls:     opts.StagedExternalCalls,
		boundary:        opts.InputBoundary,

		compactionDeferAnnounced: opts.CompactionDeferAnnounced,
		activeSubagentsProvider:  opts.ActiveSubagentsProvider,
	}
	var msStore sessionstore.RuntimeStore

	if s.store != nil {
		msStore = s.store
	}

	s.ms = newMessageStore(msStore, opts.ID)

	return s
}

func (s *svc) SetModel(model, reasoningLevel string) error {
	return s.handleSetModel(model, reasoningLevel)
}

func (s *svc) AgentTypes() *registry.Set {
	return s.agentTypes
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

func (s *svc) InjectToolNotificationOnce(
	ctx context.Context,
	deliveryID, toolName, content string,
) (bool, error) {
	if deliveryID == "" {
		return false, errors.New("inject idempotent tool notification: empty delivery id")
	}

	if pending := s.PendingExternalCalls(); len(pending) > 0 {
		return false, fmt.Errorf(
			"inject synthetic %s event: external call %s (%s) is still pending",
			toolName,
			pending[0].ID,
			pending[0].Name,
		)
	}

	return s.ms.addToolNotificationPairOnce(
		ctx,
		deliveryID,
		id.Generate(),
		toolName,
		content,
	)
}

func (s *svc) ResetContextAndInjectOnce(
	ctx context.Context,
	deliveryID, prompt string,
) (bool, error) {
	if deliveryID == "" {
		return false, errors.New("idempotent context reset: empty delivery id")
	}

	if pending := s.PendingExternalCalls(); len(pending) > 0 {
		return false, fmt.Errorf(
			"reset context: external call %s (%s) is still pending",
			pending[0].ID,
			pending[0].Name,
		)
	}

	// The opening turn is the same one a brand-new session starts from: AGENTS.md
	// header (the fresh systemPrompt already carries curated memory) plus the task.
	opening := s.openingTurn(prompt)
	fingerprint := deliveryFingerprint("context_reset", s.agentsMD, prompt)

	inserted, err := s.ms.resetToOnce(ctx, deliveryID, fingerprint, opening)
	if err != nil {
		return false, fmt.Errorf("reset transcript: %w", err)
	}

	if !inserted {
		return false, nil
	}

	// In-memory derived state drops only once the durable swap succeeded.
	s.compactionBrief = ""
	s.todoStore.Clear()
	s.loopDetector.resetWindow()
	s.resetContextBaseline()

	return true, nil
}

// BuildBlockingSubagentCompletion builds the exact tool result that resolves a
// blocking task call. Keeping this separate from the background-event builder
// makes it impossible for callers to reinterpret one delivery mode as the other.
func BuildBlockingSubagentCompletion(
	taskCallID string,
	content string,
) ([]*sessionstore.StoredMessage, []llmwire.Message, error) {
	if taskCallID == "" {
		return nil, nil, errors.New("build blocking subagent completion: task call id is required")
	}

	stored := &sessionstore.StoredMessage{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: taskCallID,
		ToolName:   tool.IDTask,
	}
	mem := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: taskCallID,
		ToolName:   tool.IDTask,
	}

	return []*sessionstore.StoredMessage{stored}, []llmwire.Message{mem}, nil
}

// BuildBackgroundSubagentCompletion builds a standalone synthetic event for a
// background child. It never answers the task call that originally launched the
// child (that call already has its launch result).
func BuildBackgroundSubagentCompletion(
	childID int64,
	content string,
) ([]*sessionstore.StoredMessage, []llmwire.Message, error) {
	if childID <= 0 {
		return nil, nil, errors.New("build background subagent completion: positive child id is required")
	}

	callID := id.Generate()
	args := json.RawMessage(fmt.Sprintf(`{"child_id":%d,"event":"completed"}`, childID))
	toolCalls := []llmwire.ToolCall{{ID: callID, Name: subagentEventTool, Arguments: args}}

	toolCallsJSON, err := json.Marshal(toolCalls)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal subagent completion tool call: %w", err)
	}

	asstStored := &sessionstore.StoredMessage{Role: llmwire.RoleAssistant, ToolCalls: toolCallsJSON}

	resultStored := &sessionstore.StoredMessage{
		Role: llmwire.RoleTool, Content: content, ToolCallID: callID, ToolName: subagentEventTool,
	}

	asstMem := llmwire.Message{Role: llmwire.RoleAssistant, ToolCalls: toolCalls}
	resultMem := llmwire.Message{
		Role: llmwire.RoleTool, Content: content, ToolCallID: callID, ToolName: subagentEventTool,
	}

	return []*sessionstore.StoredMessage{asstStored, resultStored}, []llmwire.Message{asstMem, resultMem}, nil
}

// AppendDeliveredCompletion appends already-persisted completion messages to the
// live in-memory transcript, stamping each with its store DBID. No DB write.
func (s *svc) AppendDeliveredCompletion(msgs []llmwire.Message, msgIDs []int64) {
	s.ms.appendPersisted(msgs, msgIDs)
}

func (s *svc) Close() {
	s.closeOnce.Do(func() {
		if err := s.closeLLM(); err != nil {
			logger.Named("session.close").Warn("llm_close_failed", zap.Error(err))
		}

		if s.stack != nil {
			_ = s.stack.Close()
		}
	})
}

// RequestCompaction requests a forced compaction at the next loop iteration.
// Implements the session-local compactor interface.
func (s *svc) RequestCompaction(keepRecentRounds int) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.pendingCompaction = &keepRecentRounds
}

// buildPrompt assembles the promptBuilder for a session before tool registration.
// Registry-derived sections stay empty until refreshRegistrySections runs.
func buildPrompt(
	ctx context.Context,
	p params,
	opts options,
	workDir string,
	agentConfig registry.AgentTypeConfig,
) *promptBuilder {
	basePrompt := agentConfig.Prompt +
		fmt.Sprintf(
			"\n\n# Environment\n- Working directory: %s\n- Platform: %s/%s\n- Date: %s\n- Timezone: %s\n- Each user message is prefixed with `[+elapsed DOW YYYY-MM-DD HH:MM]` showing time since last activity and current timestamp. Use this for temporal reasoning.",
			workDir,
			runtime.GOOS,
			runtime.GOARCH,
			time.Now().Format("2006-01-02 (Mon)"),
			localTimezone(),
		)

	var memoriesSection string
	if p.MemoryStore != nil && opts.ProjectID != 0 {
		memoriesSection = buildMemoriesSection(ctx, p.MemoryStore, opts.ProjectID)
	}

	return newPromptBuilder(
		basePrompt,
		memoriesSection,
		buildModelsSection(p.Config.UnifiedConfig, p.Config.Model),
		opts.ExtraSkills...,
	)
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
	registerSessionTools(filtered, s)
	s.refreshRegistrySections()
}

// refreshRegistrySections recomputes the prompt sections derived from the live tool
// registry, which the daemon extends after construction. Once per activation.
func (s *svc) refreshRegistrySections() {
	s.prompt.setToolsSection(buildToolsSection(s.registry))

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

	return s.pendingCompaction != nil
}

// setCompactionFocus records (or clears) the one-shot /compact focus.
func (s *svc) setCompactionFocus(focus string) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionFocus = focus
}

// consumePendingCompaction atomically reads and clears the pending compaction request.
func (s *svc) consumePendingCompaction() *int {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	result := s.pendingCompaction
	s.pendingCompaction = nil

	return result
}

func (s *svc) contextWindow() int {
	if cw := s.currentLLM().ContextWindow(); cw > 0 {
		return cw
	}

	return compactionThreshold
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
		s.ms.setMessages(opts.ResumeMessages)

		s.iterationOffset = opts.ResumeIteration
		if opts.CompactionBrief != "" {
			s.compactionBrief = opts.CompactionBrief
		}

		if len(opts.ResumeTodoItems) > 0 {
			s.todoStore.Replace(opts.ResumeTodoItems)
		}

		log.Info("resumed_from_db", zap.Int64("root_id", s.rootID), zap.Int("iteration", opts.ResumeIteration))

		return nil
	}

	if err := s.persistState(ctx, 0, "active"); err != nil {
		return fmt.Errorf("persist initial state: %w", err)
	}

	return nil
}
