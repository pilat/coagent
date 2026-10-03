package session

import (
	"context"
	"fmt"
	"strconv"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
)

type modelSwitch struct {
	client  llm.Client
	section sessionprompt.ModelSection
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
