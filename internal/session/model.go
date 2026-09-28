package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const compactionThreshold = 80000

var (
	_              modelRuntime = (*sessionModel)(nil)
	errModelClosed              = errors.New("session model is closed")
)

type modelRuntime interface {
	initializeClient(string)
	SetModel(string, string) error
	Chat(
		context.Context,
		string,
		[]llmwire.Message,
		[]llmwire.ToolSchema,
		...llmwire.ChatOption,
	) (*llmwire.Response, error)
	Close() error
	snapshot() modelSnapshot
	recordBaseline(context.Context, int, int, uint64)
	restoreBaseline(*sessionstore.ContextBaseline)
	clearBaseline(context.Context)
}

type baselineStore interface {
	SaveContextBaseline(context.Context, int64, sessionstore.ContextBaseline) error
	ClearContextBaseline(context.Context, int64) error
}

type modelSnapshot struct {
	model         string
	reasoning     string
	contextWindow int
	generation    uint64
	baseline      *contextBaseline
}

type sessionModel struct {
	mu              sync.RWMutex
	client          llm.Client
	model           string
	reasoning       string
	generation      uint64
	baseline        *contextBaseline
	closed          bool
	cfg             *config.Config
	newClient       func(*config.Config, string) (llm.Client, error)
	prompt          *promptBuilder
	registry        tool.Registry
	authorizer      llm.ImageAuthorizer
	store           baselineStore
	sessionID       int64
	providerSession string
}

// ResolveReasoningLevel applies the active model's effort vocabulary and defaults.
// Resolving an already-resolved level is a no-op.
func ResolveReasoningLevel(models []config.ModelEntry, modelID, requested string) (string, error) {
	for _, m := range models {
		if m.ID == modelID {
			return resolveEffort(m, requested)
		}
	}

	return "", fmt.Errorf("unknown model: %s", modelID)
}

// SetModel preserves active calls and discards their measurements after switching.
func (s *sessionModel) SetModel(modelID, reasoning string) error {
	log := logger.Named("session.model")

	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()

	if closed {
		return errModelClosed
	}

	reasoning, err := validateModelSwitch(s.cfg, modelID, reasoning)
	if err != nil {
		return err
	}

	newClient, err := s.newClient(s.cfg, modelID)
	if err != nil {
		return fmt.Errorf("create client for model %s: %w", modelID, err)
	}

	newClient.SetReasoningLevel(reasoning)

	newClient.SetSessionID(s.providerSession)

	if s.authorizer != nil {
		newClient.SetImageAuthorizer(s.authorizer)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		if closeErr := newClient.Close(); closeErr != nil {
			log.Warn("unused_llm_close_failed", zap.Error(closeErr))
		}

		return errModelClosed
	}

	// A closed owner cannot publish replacement guidance or adopt its client.
	s.prompt.setModelsSection(buildModelsSection(modelID))
	s.prompt.setModelSearch(s.registry, s.cfg.UnifiedConfig.SearchNativeActive(modelID))

	oldClient := s.client
	s.client = newClient
	s.model = modelID
	s.reasoning = reasoning
	// Another window and another tokenizer: the old measurement describes neither,
	// and a request still in flight must not write one back.
	s.baseline = nil
	s.generation++
	s.mu.Unlock()

	if err := oldClient.Close(); err != nil {
		log.Warn("old_llm_close_failed", zap.Error(err))
	}

	log.Info("model_switched", zap.String("model", modelID), zap.String("reasoning", reasoning))

	return nil
}

// Chat retains the client lease until provider I/O finishes, excluding closure.
func (s *sessionModel) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, errModelClosed
	}

	// callLLM/compaction add their own operation context. Returning the provider
	// error unchanged also preserves the established user notification text.
	//nolint:wrapcheck // wrapped at the two operation-level callers
	return s.client.Chat(ctx, system, messages, tools, opts...)
}

// Close permanently retires the owner after its in-flight calls have returned.
func (s *sessionModel) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	if err := s.client.Close(); err != nil {
		return fmt.Errorf("close LLM client: %w", err)
	}

	return nil
}

func newModelRuntime(
	client llm.Client,
	cfg *config.Config,
	factory func(*config.Config, string) (llm.Client, error),
	prompt *promptBuilder,
	registry tool.Registry,
	authorizer llm.ImageAuthorizer,
	store baselineStore,
	sessionID, rootID int64,
) modelRuntime {
	group := strconv.FormatInt(sessionID, 10)
	if sessionID != rootID {
		group = fmt.Sprintf("%d:%d", rootID, sessionID)
	}

	m := &sessionModel{
		client: client, cfg: cfg, newClient: factory, model: cfg.Model,
		prompt: prompt, registry: registry, authorizer: authorizer,
		store: store, sessionID: sessionID, providerSession: group,
	}
	prompt.setNativeSearch(cfg.UnifiedConfig.SearchNativeActive(cfg.Model))

	if authorizer != nil {
		client.SetImageAuthorizer(authorizer)
	}

	return m
}

// validateModelSwitch checks that the model ID and reasoning level are valid.
func validateModelSwitch(cfg *config.Config, modelID, reasoning string) (string, error) {
	if cfg.UnifiedConfig == nil || len(cfg.UnifiedConfig.Models) == 0 {
		return "", errors.New("no models configured")
	}

	level, err := ResolveReasoningLevel(cfg.UnifiedConfig.Models, modelID, reasoning)
	if err != nil {
		return "", err
	}

	if len(cfg.UnifiedConfig.Providers) == 0 {
		return "", errors.New("no providers configured for model switching")
	}

	return level, nil
}

// resolveEffort settles the level against what the model's catalog says it accepts.
// A model offering no effort choice carries none, rather than a level nobody honours.
func resolveEffort(m config.ModelEntry, requested string) (string, error) {
	if len(m.EffortLevels) == 0 {
		return "", nil
	}

	if requested == "" {
		return m.DefaultEffort, nil
	}

	if !slices.Contains(m.EffortLevels, requested) {
		return "", fmt.Errorf(
			"model %s does not accept reasoning level %q (accepts: %s)",
			m.ID, requested, strings.Join(m.EffortLevels, ", "),
		)
	}

	return requested, nil
}

func (s *sessionModel) snapshot() modelSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	window := s.client.ContextWindow()
	if window <= 0 {
		window = compactionThreshold
	}

	baseline := s.baseline
	if baseline != nil {
		baselineCopy := *baseline
		baseline = &baselineCopy
	}

	return modelSnapshot{
		model: s.model, reasoning: s.reasoning, contextWindow: window,
		generation: s.generation, baseline: baseline,
	}
}

// initializeClient follows successful transcript initialization; a failed open never
// changes the provider's session grouping or requested reasoning level.
func (s *sessionModel) initializeClient(reasoning string) {
	if reasoning == "" {
		reasoning = string(llm.ReasoningMedium)
	}

	s.reasoning = reasoning
	s.client.SetSessionID(s.providerSession)
	s.client.SetReasoningLevel(reasoning)
}

func (s *sessionModel) recordBaseline(ctx context.Context, promptTokens, sentCount int, generation uint64) {
	if promptTokens <= 0 {
		return
	}

	s.mu.Lock()
	if s.generation != generation {
		s.mu.Unlock()
		return
	}

	s.baseline = &contextBaseline{promptTokens: promptTokens, messageCount: sentCount}
	model := s.model
	s.mu.Unlock()

	if s.store == nil {
		return
	}

	err := s.store.SaveContextBaseline(ctx, s.sessionID, sessionstore.ContextBaseline{
		Model: model, PromptTokens: promptTokens, MessageCount: sentCount,
	})
	if err != nil {
		logger.Ctx(ctx).Named("session.context").Warn("persist_context_baseline_failed", zap.Error(err))
	}
}

func (s *sessionModel) restoreBaseline(b *sessionstore.ContextBaseline) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if b == nil || b.Model != s.model || b.PromptTokens <= 0 {
		return
	}

	s.baseline = &contextBaseline{promptTokens: b.PromptTokens, messageCount: b.MessageCount}
}

func (s *sessionModel) clearBaseline(ctx context.Context) {
	s.mu.Lock()
	s.baseline = nil
	s.mu.Unlock()

	if s.store == nil {
		return
	}

	if err := s.store.ClearContextBaseline(ctx, s.sessionID); err != nil {
		logger.Ctx(ctx).Named("session.context").Warn("clear_context_baseline_failed", zap.Error(err))
	}
}
