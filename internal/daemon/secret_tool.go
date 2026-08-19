package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/id"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// secretsDisplayPath is the secrets file path as shown to the model.
const secretsDisplayPath = "~/" + coagenthome.DirName + "/" + coagenthome.SecretsFileName

// secretNamePattern is the shape a ${VAR} reference can resolve.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var _ tool.Tool = (*requestSecretTool)(nil)

// secretRequests correlates a pending prompt with the call waiting on it. The
// value never passes through here — only the fact that someone is being asked.
type secretRequests struct {
	mu  sync.Mutex
	seq int64
	all map[string]secretRequest
}

type secretRequest struct {
	requestID string
	sessionID int64
	callID    string
	name      string
	purpose   string
	// asked orders replay, so a terminal that attaches late is prompted in the
	// order the model asked.
	asked int64
}

// requestSecretTool asks the terminal for a credential. The value goes straight
// to the secrets file over the socket; the model only ever sees the var name.
type requestSecretTool struct {
	svc       *svc
	sessionID int64
}

type requestSecretParams struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

func newSecretRequests() *secretRequests {
	return &secretRequests{all: make(map[string]secretRequest)}
}

func (r *secretRequests) add(requestID string, req secretRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	req.asked = r.seq
	req.requestID = requestID
	r.all[requestID] = req
}

// forSession lists a session's open prompts, oldest first.
func (r *secretRequests) forSession(sessionID int64) []secretRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []secretRequest

	for _, req := range r.all {
		if req.sessionID == sessionID {
			out = append(out, req)
		}
	}

	slices.SortFunc(out, func(a, b secretRequest) int { return cmp.Compare(a.asked, b.asked) })

	return out
}

// restore puts a claimed request back when its outcome could not be delivered:
// the prompt is still open, and a terminal must still be able to answer it.
func (r *secretRequests) restore(req secretRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.all[req.requestID] = req
}

// name reads the variable an open request is asking for, without claiming it.
func (r *secretRequests) name(requestID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.all[requestID]

	return req.name, ok
}

func (r *secretRequests) take(requestID string) (secretRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.all[requestID]
	delete(r.all, requestID)

	return req, ok
}

// registerSecretTool registers request_secret where the reserved configuration
// project also has a person watching through the terminal.
func (s *svc) registerSecretTool(ctx context.Context, rec *sessionstore.SessionRecord, sess session.Service) {
	if s.applier == nil || !s.isConfigurationSession(ctx, rec) || !isCLISession(rec) {
		return
	}

	registerLogged(ctx, sess, &requestSecretTool{svc: s, sessionID: rec.ID})
}

// isCLISession reports whether a session belongs to a terminal, by the attribute
// the CLI manager stamps at creation.
func isCLISession(rec *sessionstore.SessionRecord) bool {
	channel, _ := rec.Attributes["channel"].(string)
	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return channel == controllerapi.BuiltinCLIManagerID &&
		(owner == "" || owner == controllerapi.BuiltinCLIManagerID)
}

func (s *svc) isConfigurationSession(ctx context.Context, rec *sessionstore.SessionRecord) bool {
	if rec.ParentID != 0 {
		return false
	}

	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != "" && owner != controllerapi.BuiltinCLIManagerID {
		return false
	}

	name, err := s.store.GetProjectName(ctx, rec.ProjectID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.config_gate").Warn(
			"project_identity_unavailable",
			zap.Int64("project_id", rec.ProjectID),
			zap.Error(err),
		)

		return false
	}

	if name != controllerapi.CoagentSystemProjectName {
		return false
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.config_gate").Warn(
			"project_path_unavailable",
			zap.Int64("project_id", rec.ProjectID),
			zap.Error(err),
		)

		return false
	}

	return sameProjectPath(workDir, s.systemProject)
}

// ResolveSecretRequest answers the call that asked for a credential. It is given
// the variable name, never the value.
func (s *svc) ResolveSecretRequest(ctx context.Context, requestID, name string) error {
	return s.finishSecretRequest(ctx, requestID, "secret "+name+" set")
}

// CancelSecretRequest closes a masked prompt the person refused, so the model
// learns it was declined instead of the session waiting on a terminal forever.
func (s *svc) CancelSecretRequest(ctx context.Context, requestID string) error {
	name, ok := s.secrets.name(requestID)
	if !ok {
		return fmt.Errorf("no pending secret request %q", requestID)
	}

	return s.finishSecretRequest(ctx, requestID, "the user declined to provide "+name+
		"; do not ask again unless they bring it up")
}

// PendingSecretRequests lists the prompts this session is still waiting on, in
// the order they were asked. A push is delivered once, so a terminal that
// attaches later has no other way to learn a prompt is open.
func (s *svc) PendingSecretRequests(sessionID int64) []sessionevent.Notification {
	open := s.secrets.forSession(sessionID)

	out := make([]sessionevent.Notification, 0, len(open))
	for _, req := range open {
		out = append(out, sessionevent.Notification{
			Type:       sessionevent.NotifySecretRequest,
			RequestID:  req.requestID,
			SecretName: req.name,
			Message:    req.purpose,
		})
	}

	return out
}

func (s *svc) finishSecretRequest(ctx context.Context, requestID, content string) error {
	req, ok := s.secrets.take(requestID)
	if !ok {
		return fmt.Errorf("no pending secret request %q", requestID)
	}

	// The ledger entry is what a session rebuilt for this delivery checks before
	// it accepts the result; the runner drops it once the result is in.
	if _, err := s.DeliverPendingCallResult(
		ctx, req.sessionID, req.callID, tool.IDRequestSecret, content,
	); err != nil {
		s.secrets.restore(req)

		return fmt.Errorf("deliver secret outcome: %w", err)
	}

	// take is the single-winner claim, so this fires once however many terminals
	// were showing the prompt — and the losers only ever heard the opening push.
	s.NotifySession(req.sessionID, sessionevent.Notification{
		Type:       sessionevent.NotifySecretResolved,
		RequestID:  req.requestID,
		SecretName: req.name,
	})

	return nil
}

func (t *requestSecretTool) ID() string { return tool.IDRequestSecret }

func (t *requestSecretTool) Description() string {
	return "Ask the person at the terminal to type a credential. They see a masked prompt; you see only " +
		"the variable name, and the value goes straight into " + secretsDisplayPath + ". Use this whenever a " +
		"provider key or bot token is needed — never ask for one in the chat, where it would be stored " +
		"in the conversation forever. Reference the result as ${NAME} afterwards."
}

func (t *requestSecretTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Variable to store it under, e.g. \"MANAGER_TG_BOT_TOKEN\". Upper snake case."},
    "purpose": {"type": "string", "description": "One line telling the user what this credential is for, shown above the prompt."}
  },
  "required": ["name", "purpose"]
}`)
}

func (t *requestSecretTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p requestSecretParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse parameters: %w", err)
	}

	if !secretNamePattern.MatchString(p.Name) {
		return nil, errors.New("a secret name must look like AN_ENV_VAR")
	}

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("no tool_call id to answer against")
	}

	requestID := id.Generate()
	t.svc.secrets.add(requestID, secretRequest{
		sessionID: t.sessionID, callID: callID, name: p.Name, purpose: p.Purpose,
	})
	t.svc.staged.stage(t.sessionID, callID, tool.IDRequestSecret)

	t.svc.NotifySession(t.sessionID, sessionevent.Notification{
		Type:       sessionevent.NotifySecretRequest,
		RequestID:  requestID,
		SecretName: p.Name,
		Message:    p.Purpose,
	})

	return nil, tool.ErrSuspend
}

// builtinSkillsFor picks the embedded skills a session gets. The onboarding
// guide goes only to a terminal chat: its script calls request_secret, and a
// skill that tells a Telegram session to use a tool it does not have is worse
// than no skill at all.
func (s *svc) builtinSkillsFor(ctx context.Context, rec *sessionstore.SessionRecord) []*loader.Skill {
	if !s.isConfigurationSession(ctx, rec) || !isCLISession(rec) {
		return nil
	}

	skill, err := loader.BuiltinSkill(loader.OnboardingSkillName)
	if err != nil {
		logger.Ctx(ctx).Named("daemon").Warn("builtin_skill_unavailable", zap.Error(err))

		return nil
	}

	return []*loader.Skill{skill}
}
