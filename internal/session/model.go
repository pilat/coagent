package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

// ResolveReasoningLevel settles a requested effort level the way a live model
// switch does. Callers outside the loop use it so what they record is what a
// session will ask for. Resolving an already-resolved level is a no-op.
func ResolveReasoningLevel(models []config.ModelEntry, modelID, requested string) (string, error) {
	for _, m := range models {
		if m.ID == modelID {
			return resolveEffort(m, requested)
		}
	}

	return "", fmt.Errorf("unknown model: %s", modelID)
}

// validateModelSwitch checks that the model ID and reasoning level are valid.
func (s *svc) validateModelSwitch(modelID, reasoning string) (string, error) {
	if s.cfg.UnifiedConfig == nil || len(s.cfg.UnifiedConfig.Models) == 0 {
		return "", errors.New("no models configured")
	}

	level, err := ResolveReasoningLevel(s.cfg.UnifiedConfig.Models, modelID, reasoning)
	if err != nil {
		return "", err
	}

	if len(s.cfg.UnifiedConfig.Providers) == 0 {
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

// handleSetModel switches the LLM model mid-session.
func (s *svc) handleSetModel(modelID, reasoning string) error {
	reasoning, err := s.validateModelSwitch(modelID, reasoning)
	if err != nil {
		return err
	}

	newClient, err := s.newLLMWithModel(s.cfg, modelID)
	if err != nil {
		return fmt.Errorf("create client for model %s: %w", modelID, err)
	}

	newClient.SetReasoningLevel(reasoning)

	// Preserve session ID for OpenRouter UI grouping
	sessionID := strconv.FormatInt(s.id, 10)
	if s.id != s.rootID {
		sessionID = fmt.Sprintf("%d:%d", s.rootID, s.id)
	}

	newClient.SetSessionID(sessionID)

	// promptBuilder is self-synchronized, so this needs no modelMu.
	s.prompt.setModelsSection(buildModelsSection(modelID))
	// Search guidance follows the active client: a switch can flip the native
	// passthrough on or off (and the section with it).
	s.prompt.setModelSearch(s.registry, s.cfg.UnifiedConfig.SearchNativeActive(modelID))

	// Swap the triplet under modelMu (the loop reads it via currentLLM /
	// buildSessionStatus); close the old client outside the lock — Close is IO.
	s.modelMu.Lock()
	oldClient := s.llmClient
	s.llmClient = newClient
	s.model = modelID
	s.reasoningLevel = reasoning
	// Another window and another tokenizer: the old measurement describes neither,
	// and a request still in flight must not write one back.
	s.baseline = nil
	s.modelEpoch++
	s.modelMu.Unlock()

	log := logger.Named("session.model")
	if err := oldClient.Close(); err != nil {
		log.Warn("old_llm_close_failed", zap.Error(err))
	}

	log.Info("model_switched", zap.String("model", modelID), zap.String("reasoning", reasoning))

	return nil
}

// chat holds a read lease for the entire provider call. handleSetModel must take
// the write lock before swapping, so it cannot close the old client until every
// in-flight Chat using that client has returned.
func (s *svc) chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	// callLLM/compaction add their own operation context. Returning the provider
	// error unchanged also preserves the established user notification text.
	//nolint:wrapcheck // wrapped at the two operation-level callers
	return s.llmClient.Chat(ctx, system, messages, tools, opts...)
}

// closeLLM excludes both model swaps and in-flight provider calls while the
// session releases the current client.
func (s *svc) closeLLM() error {
	s.modelMu.Lock()
	defer s.modelMu.Unlock()

	if err := s.llmClient.Close(); err != nil {
		return fmt.Errorf("close LLM client: %w", err)
	}

	return nil
}

// currentLLM returns a short-lived snapshot for non-resource operations such as
// reading ContextWindow. Resource-using calls go through chat/closeLLM instead.
func (s *svc) currentLLM() llm.Client {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()

	return s.llmClient
}
