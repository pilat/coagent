package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

const compactionThreshold = 80000

// Events receives live, non-durable notifications for the controller.
type Events interface {
	Emit(sessionevent.Notification)
}

// Input carries the assembled dependencies and durable activation snapshot.
type Input struct {
	ImageAuthorizer          llm.ImageAuthorizer
	Record                   *sessionstore.SessionRecord
	Client                   llm.Client
	Loader                   loader.Service
	Registry                 tool.Registry
	Prompt                   *sessionprompt.Builder
	Store                    Store
	Events                   Events
	ExternalCalls            map[string]string
	OpeningContext           string
	OutputEnabled            bool
	PreserveStopped          bool
	Schedules                string
	Status                   string
	BackgroundSnapshot       string
	CompactionDeferAnnounced bool
}

// Session executes one activation over an assembled tool and model stack.
type Session struct {
	imageAuthorizer   llm.ImageAuthorizer
	workDir           string
	projectID         int64
	llmClient         llm.Client
	loader            loader.Service
	registry          tool.Registry
	activationIndex   map[string]string
	currentActivation *tool.ActivationGrant
	agentsMD          string
	store             Store
	rootID            int64
	id                int64
	model             string
	iterationOffset   int
	reasoningLevel    string
	// modelMu orders queued clients and context measurements with model adoption.
	modelMu         sync.RWMutex
	closeOnce       sync.Once
	stamper         sessionprompt.Timestamper
	prompt          *sessionprompt.Builder
	events          Events
	schedules       string
	status          string
	outputEnabled   bool
	preserveStopped bool
	budgetFired     bool

	// Loop execution state
	ms           *messageStore
	loopDetector *loopDetector
	// Durable completion state is re-read each turn; this only seeds the cache.
	resumeCompletion  *sessionstore.CompletionCheckState
	compactionFocus   string // optional /compact focus, set for one compact() then cleared
	pendingCompaction bool
	compactionInput   *PendingInput
	// Summary row of the last auto compaction; keys its success output
	// (`compaction:<id>:succeeded`) so a replay is an idempotent no-op.
	compactionSummaryDBID int64
	compactionOutputs     []*sessionstore.OutputCommit
	// compactionDeferAnnounced survives the session object: the daemon rebuilds
	// this Session on every wake, so per-run state would re-announce per wake.
	compactionDeferAnnounced bool
	suspended                bool
	// stagedCalls are tool_call ids the daemon has already started outside work
	// for (call id → tool name). Loop-read only; set once at construction.
	stagedCalls              map[string]string
	activeBackgroundSnapshot string
	// Under modelMu with the model triplet: a measurement describes one model's
	// window and tokenizer. nil baseline = nothing measured.
	baseline     *contextBaseline
	pendingModel *modelSwitch
	modelClosed  bool
	modelEpoch   uint64
}

type modelSwitch struct {
	client  llm.Client
	section sessionprompt.ModelSection
}

// Store is the persistence view required by session.
type Store interface {
	Commit(context.Context, sessionstore.Commit) (*sessionstore.CommitResult, error)
	ListPending(context.Context, int64) ([]*sessionstore.InboxInput, error)
	PendingActivation(context.Context, int64) (*sessionstore.ToolActivation, error)

	HasBackgroundWakeSource(context.Context, int64) (bool, error)
	LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error)
	LoadCompletionCheckState(context.Context, int64) (*sessionstore.CompletionCheckState, error)
	HasOutstandingResponseRecovery(context.Context, int64) (bool, error)
	GetChildSessionStats(context.Context, int64) (int, int, error)
	GetSessionTreeUsage(context.Context, int64) (int, int, float64, error)
	LookupRead(ctx context.Context, sessionID int64, path string) (sessionstore.FileReadRecord, bool, error)
	RecordRead(ctx context.Context, sessionID int64, path string, record sessionstore.FileReadRecord) error
}

// New installs a prepared session and restores its durable transcript.
func New(ctx context.Context, in Input) (*Session, error) {
	r := in.Record

	s := &Session{
		imageAuthorizer:          in.ImageAuthorizer,
		workDir:                  in.Prompt.WorkDir,
		projectID:                r.ProjectID,
		id:                       r.ID,
		rootID:                   r.RootID,
		llmClient:                in.Client,
		loader:                   in.Loader,
		registry:                 in.Registry,
		prompt:                   in.Prompt,
		agentsMD:                 in.OpeningContext,
		store:                    in.Store,
		model:                    r.Model,
		reasoningLevel:           in.Client.GetReasoningLevel(),
		stamper:                  sessionprompt.NewTimestamper(r.UpdatedAt),
		loopDetector:             newLoopDetector(),
		stagedCalls:              in.ExternalCalls,
		schedules:                in.Schedules,
		status:                   in.Status,
		events:                   in.Events,
		outputEnabled:            in.OutputEnabled,
		preserveStopped:          in.PreserveStopped,
		activeBackgroundSnapshot: in.BackgroundSnapshot,
		compactionDeferAnnounced: in.CompactionDeferAnnounced,
		iterationOffset:          r.Iteration,
	}
	if s.rootID == 0 {
		s.rootID = s.id
	}

	s.ms = newMessageStore(in.Store, r.ID)
	if err := s.ms.reloadMessages(ctx); err != nil {
		return nil, fmt.Errorf("restore transcript: %w", err)
	}

	s.installPersistedBaseline(r.ContextBaseline())
	s.seedResumeCompletion(
		&sessionstore.CompletionCheckState{
			CandidateID:         r.CompletionCheckCandidateID,
			ManagerReplyPending: r.ManagerReplyPending,
			EmptyStopStreak:     r.EmptyStopStreak,
		},
	)

	if len(s.ms.getMessages()) == 0 {
		if err := s.persistState(ctx, 0, sessionstore.SessionStatusActive); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// Close releases model clients after the activation has joined.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if err := s.closeLLM(); err != nil {
			logger.Named("session.close").Warn("llm_close_failed", zap.Error(err))
		}
	})
}

// RequestCompaction queues a forced checkpoint for the next boundary.
func (s *Session) RequestCompaction() {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.pendingCompaction = true
}

// SwitchModel transfers a prepared client for the next model step.
func (s *Session) SwitchModel(client llm.Client, modelsSection sessionprompt.ModelSection) {
	s.modelMu.Lock()
	if s.modelClosed {
		s.modelMu.Unlock()

		_ = client.Close()

		return
	}

	previous := s.pendingModel
	client.SetImageAuthorizer(s.imageAuthorizer)
	s.pendingModel = &modelSwitch{client: client, section: modelsSection}
	s.modelMu.Unlock()

	if previous != nil {
		_ = previous.client.Close()
	}
}

func (s *Session) PrepareUserMessage(message string) (string, error) {
	prepared, err := s.PrepareUserMessageDetailed(message)
	return prepared.Content, err
}

func (s *Session) PrepareUserMessageDetailed(message string) (sessionprompt.PreparedMessage, error) {
	prepared, err := sessionprompt.PrepareUserMessageDetailed(s.loader, message)
	if err != nil {
		return prepared, fmt.Errorf("prepare user message: %w", err)
	}

	return prepared, nil
}

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

func (s *Session) compactionCommandInput() *PendingInput {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	if s.compactionInput == nil {
		return nil
	}

	input := *s.compactionInput

	return &input
}

func (s *Session) setCompactionCommandInput(input PendingInput) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionInput = &input
}

func (s *Session) clearCompactionCommandInput() {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionInput = nil
}

// setCompactionFocus records (or clears) the one-shot /compact focus.
func (s *Session) setCompactionFocus(focus string) {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	s.compactionFocus = focus
}

// consumePendingCompaction atomically reads and clears the pending compaction request.
func (s *Session) consumePendingCompaction() bool {
	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	pending := s.pendingCompaction
	s.pendingCompaction = false

	return pending
}

func (s *Session) contextWindow() int {
	if cw := s.currentLLM().ContextWindow(); cw > 0 {
		return cw
	}

	return compactionThreshold
}

func (s *Session) seedResumeCompletion(state *sessionstore.CompletionCheckState) {
	s.resumeCompletion = state
}

func (s *Session) persistState(ctx context.Context, iteration int, status sessionstore.SessionStatus) error {
	raw, err := json.Marshal(s.prompt.Todos.List())
	if err != nil {
		return fmt.Errorf("persist state: %w", err)
	}

	data := json.RawMessage(raw)
	c := s.newCommit()
	c.State = sessionstore.StatePatch{Iteration: &iteration, Status: &status, TodoItems: &data}
	_, err = s.commit(ctx, c)

	return err
}

func (s *Session) applyModelSwitch() {
	s.modelMu.Lock()

	pending := s.pendingModel
	if pending == nil {
		s.modelMu.Unlock()
		return
	}

	s.pendingModel = nil
	old := s.llmClient
	s.llmClient = pending.client
	s.model = pending.section.ID
	s.reasoningLevel = pending.client.GetReasoningLevel()
	s.baseline = nil
	s.modelEpoch++

	sessionID := strconv.FormatInt(s.id, 10)
	if s.id != s.rootID {
		sessionID = fmt.Sprintf("%d:%d", s.rootID, s.id)
	}

	s.llmClient.SetSessionID(sessionID)
	s.prompt.SetModelsSection(pending.section.Text)
	s.prompt.SetModelSearch(s.registry, pending.section.NativeSearch)
	s.modelMu.Unlock()

	if err := old.Close(); err != nil {
		logger.Named("session.model").Warn("old_llm_close_failed", zap.Error(err))
	}

	s.emit(sessionevent.Notification{Type: sessionevent.NotifyContextChanged})
}

// Only the loop adopts queued clients, and Close runs after the loop joins.
// Holding a model lease over IO would delay command handling until Chat returns.
func (s *Session) chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	//nolint:wrapcheck // wrapped at the two operation-level callers
	return s.currentLLM().Chat(ctx, system, messages, tools, opts...)
}

// The joined loop cannot use a client after this closes adoption.
func (s *Session) closeLLM() error {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	s.modelClosed = true
	if s.pendingModel != nil {
		_ = s.pendingModel.client.Close()
		s.pendingModel = nil
	}

	if err := s.llmClient.Close(); err != nil {
		return fmt.Errorf("close LLM client: %w", err)
	}

	return nil
}

// currentLLM returns a short-lived snapshot for non-resource operations such as
// reading ContextWindow. Resource-using calls go through chat/closeLLM instead.
func (s *Session) currentLLM() llm.Client {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.llmClient
}

func (s *Session) renderSessionHelp() string {
	lines := []string{
		"## Session commands",
		"`/status` — show session status",
		"`/stop` — stop the current run",
		"`/clear` — start a fresh session",
		"`/kill` — close this session",
		"`/compact [focus]` — compact the context",
		"`/schedules` — list schedules",
		"`/budget <request>` — arm, replace, inspect, or clear a one-shot cost/wall-time checkpoint",
		"`/gwt <name>` — fork into a worktree (Telegram session topics only)",
	}
	if s.loader == nil {
		return strings.Join(lines, "\n")
	}

	for _, skill := range s.loader.ListUserInvocableSkills() {
		line := "`/skill " + skill.Name + "`"
		if skill.Description != "" {
			line += " — " + skill.Description
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}
