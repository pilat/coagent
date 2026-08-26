package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
)

// Chat op names. Session ids make reconnects explicit, but an operation may
// address only the session this manager adopted.
const (
	OpChatOpen     = "chat_open"
	OpChatSend     = "chat_send"
	OpChatStop     = "chat_stop"
	OpChatModels   = "chat_models"
	OpChatSetModel = "chat_set_model"
)

type (
	// Event is one line of chat output pushed to every attached terminal.
	Event struct {
		SessionID     int64  `json:"session_id"`
		OldSessionID  int64  `json:"old_session_id,omitempty"`
		Generation    int64  `json:"generation,omitempty"`
		AfterOutputID int64  `json:"after_output_id,omitempty"`
		Type          string `json:"type"`
		Message       string `json:"message,omitempty"`
		Status        string `json:"status,omitempty"`
	}

	// SecretRequest asks the terminal for one credential. The value never comes
	// back through the chat stream — it goes straight to the set_secret op.
	SecretRequest struct {
		SessionID int64  `json:"session_id"`
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
		Purpose   string `json:"purpose,omitempty"`
	}

	// SecretResolved tells a terminal one masked prompt is over, whoever answered
	// it. No credential is involved either way — only the request it closes.
	SecretResolved struct {
		SessionID int64  `json:"session_id"`
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
	}

	// OpenResult answers chat_open. A zero session id means the conversation has
	// not started yet: the first message creates it.
	OpenResult struct {
		SessionID  int64  `json:"session_id"`
		Generation int64  `json:"generation,omitempty"`
		WorkDir    string `json:"work_dir"`
	}

	// SendParams is one message from a terminal. A zero session id asks for the
	// conversation to be started.
	SendParams struct {
		SessionID int64  `json:"session_id"`
		Text      string `json:"text"`
		Model     string `json:"model,omitempty"`
	}

	// SendResult reports which session took the message, so a client that opened
	// on nothing learns the id it now belongs to.
	SendResult struct {
		SessionID  int64 `json:"session_id"`
		Generation int64 `json:"generation,omitempty"`
	}

	// SessionParams names the session an op acts on.
	SessionParams struct {
		SessionID int64 `json:"session_id"`
	}
)

func (m *Manager) registerOps() error {
	ops := []struct {
		name    string
		handler ctl.Handler
	}{
		{OpChatOpen, m.openHandler()},
		{OpChatSend, m.sendHandler()},
		{OpChatStop, m.stopHandler()},
		{OpChatModels, m.modelsHandler()},
		{OpChatSetModel, m.setModelHandler()},
		{OpChatSecretCancel, m.cancelSecretHandler()},
	}

	for _, op := range ops {
		if err := registerHandler(m.server, op.name, op.handler); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) openHandler() ctl.Handler {
	return func(ctx context.Context, c *ctl.Conn, _ json.RawMessage) (any, *ctl.Error) {
		res, err := m.open(ctx, c)
		if err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		return res, nil
	}
}

func (m *Manager) sendHandler() ctl.Handler {
	return func(ctx context.Context, c *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p SendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		res, err := m.send(ctx, c, p)
		if err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		return res, nil
	}
}

func (m *Manager) stopHandler() ctl.Handler {
	return func(ctx context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p SessionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		if err := m.requireOwnedSession(p.SessionID); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		acceptedID, err := m.deliver(ctx, p.SessionID, "/stop")
		if err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		_, generation := m.lifecycle()

		return SendResult{SessionID: acceptedID, Generation: generation}, nil
	}
}

func registerHandler(server *ctl.Server, op string, handler ctl.Handler) error {
	if err := server.Register(op, handler); err != nil {
		return fmt.Errorf("register %s: %w", op, err)
	}

	return nil
}

// open attaches a terminal to the chat and resolves which session it is joining:
// the project's most recent one whatever state it ended in — a conversation
// continues, it does not restart because the last answer happened to be an error.
func (m *Manager) open(ctx context.Context, c *ctl.Conn) (OpenResult, error) {
	t := m.attach(c)
	if m.delivery != nil {
		if queue, ok := m.controller.(controllerapi.OutputQueueController); ok {
			_ = queue.WakeOutput(ctx)
		}

		m.delivery.Wake()
	}

	sessionID, err := m.resumeLatest(ctx)
	if err != nil {
		return OpenResult{}, err
	}

	m.adopt(sessionID)
	m.replaySecretRequests(t, sessionID)

	m.mu.Lock()
	workDir := m.workDir
	m.mu.Unlock()

	_, generation := m.lifecycle()

	return OpenResult{SessionID: sessionID, Generation: generation, WorkDir: workDir}, nil
}

// send routes one normal message through the controller's durable input path.
//
//nolint:wsl_v5 // Closed-ID normalization precedes the existing owned-session admission path.
func (m *Manager) send(ctx context.Context, c *ctl.Conn, p SendParams) (SendResult, error) {
	if strings.TrimSpace(p.Text) == "" {
		return SendResult{}, errors.New("nothing to send")
	}

	m.attach(c)

	// The decision to create and the creation itself are one step: two terminals
	// racing the first message would otherwise both see "no session" and start
	// two, orphaning one of them.
	m.createMu.Lock()
	defer m.createMu.Unlock()

	sessionID := p.SessionID
	if sessionID != 0 && m.closedSession(sessionID) {
		sessionID = 0
	}
	if sessionID != 0 && !m.ownsSession(sessionID) {
		// The replacement aliases are a delivery cache, not the authority:
		// until the lifecycle push lands, only the controller knows where an
		// old ID resolves, and it fails closed for foreign owners itself.
		if _, routed := m.controller.(controllerapi.SessionMessageRouter); !routed {
			return SendResult{}, fmt.Errorf("session %d is not owned by the local chat", sessionID)
		}
	}

	applyPendingModel := sessionID == 0 && p.Model != ""
	if sessionID == 0 {
		m.mu.Lock()
		sessionID = m.sessionID
		m.mu.Unlock()
	}

	if sessionID == 0 {
		return m.create(ctx, p.Text, p.Model)
	}

	if applyPendingModel {
		if err := m.controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
			SessionID: sessionID,
			Model:     p.Model,
		}); err != nil {
			return SendResult{}, fmt.Errorf("apply pending chat model: %w", err)
		}
	}

	_, generation := m.lifecycle()

	acceptedID, err := m.deliver(ctx, sessionID, p.Text)
	if err != nil {
		return SendResult{}, err
	}

	m.adoptIfUnchanged(acceptedID, generation)

	_, generation = m.lifecycle()

	return SendResult{SessionID: acceptedID, Generation: generation}, nil
}

func (m *Manager) requireOwnedSession(sessionID int64) error {
	if !m.ownsSession(sessionID) {
		return fmt.Errorf("session %d is not owned by the local chat", sessionID)
	}

	return nil
}

func (m *Manager) deliver(ctx context.Context, sessionID int64, text string) (int64, error) {
	data := controllerapi.SessionMessageData{SessionID: sessionID, Message: text}
	if router, ok := m.controller.(controllerapi.SessionMessageRouter); ok {
		acceptedID, err := router.SendSessionMessageResolved(ctx, data)
		if err != nil {
			return 0, fmt.Errorf("send resolved session message: %w", err)
		}

		return acceptedID, nil
	}

	if err := m.controller.SendSessionMessage(ctx, data); err != nil {
		return 0, fmt.Errorf("send session message: %w", err)
	}

	return sessionID, nil
}

func (m *Manager) create(ctx context.Context, prompt, model string) (SendResult, error) {
	m.mu.Lock()
	workDir := m.workDir
	m.mu.Unlock()

	if model == "" {
		model = m.model
	}

	id, err := m.controller.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir:       workDir,
		Prompt:        prompt,
		Model:         model,
		Attributes:    map[string]any{channelAttribute: channelCLI},
		SystemProject: controllerapi.CoagentSystemProjectName,
	})
	if err != nil {
		return SendResult{}, fmt.Errorf("create chat session: %w", err)
	}

	m.adopt(id)

	_, generation := m.lifecycle()

	return SendResult{SessionID: id, Generation: generation}, nil
}

// resumeLatest finds the chat project's most recent live session, or 0 when
// there is none to continue.
func (m *Manager) resumeLatest(ctx context.Context) (int64, error) {
	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}

	m.mu.Lock()
	projectID := m.projectID
	m.mu.Unlock()

	var latest controllerapi.SessionInfo

	for _, s := range sessions {
		if s.ProjectID != projectID || s.KilledAt != nil {
			continue
		}

		owner, _ := s.Attributes[controllerapi.SessionAttributeManagerID].(string)

		channel, _ := s.Attributes[channelAttribute].(string)

		if owner != "" && owner != m.ID() || owner == "" && channel != channelCLI {
			continue
		}

		if latest.ID == 0 || s.UpdatedAt.After(latest.UpdatedAt) {
			latest = s
		}
	}

	if latest.ID == 0 {
		return 0, nil
	}

	if owner, _ := latest.Attributes[controllerapi.SessionAttributeManagerID].(string); owner == "" {
		attributes := maps.Clone(latest.Attributes)

		attributes[controllerapi.SessionAttributeManagerID] = m.ID()

		if err := m.controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
			SessionID: latest.ID, Attributes: attributes,
		}); err != nil {
			return 0, fmt.Errorf("claim legacy chat session: %w", err)
		}
	}

	return latest.ID, nil
}
