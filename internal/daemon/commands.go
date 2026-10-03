package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

const (
	stopCommand    = "/stop"
	clearCommand   = "/clear"
	killCommand    = "/kill"
	compactCommand = "/compact"
)

func (s *svc) Send(ctx context.Context, projectID int64, prompt, model string, attrs map[string]any) (int64, error) {
	return s.send(ctx, projectID, prompt, model, attrs)
}

func (s *svc) SendToSession(ctx context.Context, sessionID int64, prompt string) error {
	input, err := s.enqueueUserSessionInput(ctx, sessionID, prompt)
	if err != nil {
		if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
			if record, getErr := s.store.GetSession(ctx, sessionID); getErr == nil && record.KilledAt != nil {
				return fmt.Errorf("session %d is killed", sessionID)
			}
		}

		// The park drain won the arbitration CAS; the retry is the user's next
		// message once the parked root accepts input again.
		if errors.Is(err, budget.ErrConflict) {
			return fmt.Errorf(
				"session %d is parking after a budget checkpoint — send the message again once it stops",
				sessionID,
			)
		}

		return fmt.Errorf("persist session input: %w", err)
	}

	if handled, err := s.handleGenericCommand(ctx, input); handled || err != nil {
		return err
	}

	_, ok := s.runners.Load(sessionID)
	if ok {
		return nil
	}

	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if rec.KilledAt != nil {
		return fmt.Errorf("session %d is killed", sessionID)
	}

	if rec.Status == sessionstore.SessionStatusStopping {
		return fmt.Errorf("session %d is stopping", sessionID)
	}

	parked, err := s.prepareStoppedSessionInput(ctx, rec, prompt)
	if err != nil {
		return err
	}

	if parked {
		return nil
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	if _, ok = s.runners.Load(sessionID); ok {
		return nil
	}

	if err := s.ensureRunner(ctx, sessionID, workDir, rec.ProjectID); err != nil {
		if errors.Is(err, admission.ErrNoCapacity) {
			s.enqueuePendingRunner(sessionID, workDir, rec.ProjectID)
			return nil
		}

		return err
	}

	return nil
}

func (s *svc) SendToSessionResolved(ctx context.Context, sessionID int64, prompt string) (int64, error) {
	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("load session for replacement resolution: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	resolved, err := s.store.ResolveReplacement(ctx, sessionID, owner)
	if err != nil {
		return 0, fmt.Errorf("resolve replacement session: %w", err)
	}

	sessionID = resolved

	if err := s.SendToSession(ctx, sessionID, prompt); err != nil {
		return 0, err
	}

	return sessionID, nil
}

// Model publication and construction share the tree fence so the record and
// the next activation cannot disagree about which client to adopt.
func (s *svc) SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error {
	if err := s.checkModelConfigured(model); err != nil {
		return err
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for model switch: %w", err)
	}
	if s.budgetSvc != nil {
		budgetRecord, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(record))
		if budgetErr == nil && budgetRecord.State == budget.Armed &&
			budgetRecord.CostLimitUSD != nil && !s.modelHasPricing(model) {
			return errors.New("cannot switch an armed budget tree to a model without catalog pricing")
		}

		if budgetErr != nil && !errors.Is(budgetErr, budget.ErrNotFound) {
			return fmt.Errorf("load budget for model switch: %w", budgetErr)
		}
	}

	client, section, err := sessionbuild.BuildClient(s.buildInput.Config, model, reasoningLevel)
	if err != nil {
		return fmt.Errorf("construct model %s: %w", model, err)
	}
	level := client.GetReasoningLevel()
	if err := s.store.UpdateSessionModel(ctx, sessionID, model, level); err != nil {
		_ = client.Close()
		return fmt.Errorf("update session model: %w", err)
	}
	rs, ok := s.runners.Load(sessionID)
	if ok {
		if sess := rs.Service(); sess != nil {
			sess.SwitchModel(client, section)
			return nil
		}
	}
	_ = client.Close()

	return nil
}

func (s *svc) SetAttributes(ctx context.Context, sessionID int64, attrs map[string]any) error {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	rec, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session before setting attributes: %w", err)
	}

	if rec == nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	attrs = maps.Clone(attrs)
	existingOwner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)

	requestedOwner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	claimingOwner := existingOwner == "" && requestedOwner != ""

	if claimingOwner && (rec.Status == sessionstore.SessionStatusTerminating || rec.KilledAt != nil) {
		return fmt.Errorf("session %d is closing and cannot acquire a manager owner", sessionID)
	}

	if existingOwner != "" && requestedOwner != "" && existingOwner != requestedOwner {
		return fmt.Errorf("session %d belongs to manager %q", sessionID, existingOwner)
	}

	if existingOwner != "" {
		if attrs == nil {
			attrs = make(map[string]any)
		}

		attrs[controllerapi.SessionAttributeManagerID] = existingOwner
		requestedOwner = existingOwner
	}

	if err := s.store.SetAttributes(ctx, sessionID, attrs); err != nil {
		return fmt.Errorf("set session attributes: %w", err)
	}

	s.childMu.Lock()
	s.ownerCache[sessionID] = requestedOwner
	s.childMu.Unlock()

	return nil
}

func isReadOnlyBoundaryCommand(content string) bool {
	content = strings.TrimSpace(content)

	return content == "/status" || content == "/help" || content == "/schedules" ||
		content == compactCommand || strings.HasPrefix(content, compactCommand+" ")
}

func isExactControlCommand(content string) bool {
	content = strings.TrimSpace(content)

	return isReadOnlyBoundaryCommand(content) || content == stopCommand || content == clearCommand ||
		content == killCommand
}

//nolint:funcorder // Command dispatch remains beside durable input admission and lifecycle fencing.
func (s *svc) handleGenericCommand(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	if input.Source != sessionstore.InputSourceUser {
		return false, nil
	}

	if strings.TrimSpace(input.RawContent) == "/status" {
		return true, s.handleStatusInput(ctx, input)
	}

	command := strings.TrimSpace(input.RawContent)
	if command != stopCommand && command != clearCommand && command != killCommand {
		return false, nil
	}

	unlock, err := s.lockSessionTree(ctx, input.SessionID)
	if err != nil {
		return true, err
	}
	defer unlock()

	switch command {
	case stopCommand:
		record, err := s.store.GetSession(ctx, input.SessionID)
		if err != nil {
			return true, fmt.Errorf("load stop session: %w", err)
		}

		if record.Status == sessionstore.SessionStatusStopped {
			return true, s.handleStoppedStop(ctx, input)
		}

		if err := s.handleLifecycleInput(ctx, input, "⏳ Stopping…"); err != nil {
			return true, err
		}

		return true, s.stopLocked(ctx, input.SessionID, input.ID)
	case clearCommand:
		if _, err := s.clearLocked(ctx, input.SessionID, input.ID); err != nil {
			return true, err
		}

		return true, nil
	case killCommand:
		if err := s.handleLifecycleInput(ctx, input, "Stopping session..."); err != nil {
			return true, err
		}

		return true, s.killLocked(ctx, input.SessionID)
	default:
		return false, nil
	}
}

//nolint:funcorder // Immediate status dispatch belongs beside the generic command boundary.
func (s *svc) handleStatusInput(ctx context.Context, input *sessionstore.InboxInput) error {
	current, err := s.CurrentProgress(ctx, input.SessionID)
	if err != nil {
		return err
	}
	_, err = s.store.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "status command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{Type: sessionstore.OutputMessagePersistent, Content: current.Rendered, MessageRef: -1},
		},
	})
	if errors.Is(err, sessionstore.ErrInputResolved) {
		return nil
	}
	if err != nil {
		return err
	}
	s.publish(input.SessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: current.Rendered})
	if !s.HasActiveLoop(input.SessionID) {
		s.publish(
			input.SessionID,
			sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle},
		)
	}
	return nil
}

//nolint:funcorder // The idempotent stop result is part of the same command dispatcher.
func (s *svc) handleStoppedStop(ctx context.Context, input *sessionstore.InboxInput) error {
	_, err := s.store.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "stop command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{
				Type:       sessionstore.OutputMessagePersistent,
				Content:    "Session already stopped.",
				Key:        fmt.Sprintf("input:%d:stop:already_stopped", input.ID),
				MessageRef: -1,
			},
		},
	})
	return err
}

//nolint:funcorder // Lifecycle input must stay with the generic dispatcher that invokes it.
func (s *svc) handleLifecycleInput(ctx context.Context, input *sessionstore.InboxInput, content string) error {
	command := strings.TrimPrefix(strings.TrimSpace(input.RawContent), "/")

	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); !owned {
		if _, err := s.store.Commit(
			ctx,
			sessionstore.Commit{
				SessionID: input.SessionID,
				Accept: []sessionstore.Accept{
					{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: command, LinkRef: -1},
				},
			},
		); err != nil {
			return fmt.Errorf("handle lifecycle input: %w", err)
		}

		return nil
	}

	if _, err := s.store.BeginLifecycleInput(ctx, input.ID, command, content); err != nil {
		return fmt.Errorf("start lifecycle input: %w", err)
	}

	return nil
}

//nolint:funcorder // Daemon producers share this helper with the adjacent command boundary.
func (s *svc) enqueuePersistentOutput(ctx context.Context, sessionID int64, content string) error {
	outputs := s.store
	if outputs == nil {
		return nil
	}

	if _, err := outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: sessionID, Type: sessionstore.OutputMessagePersistent, Content: content,
	}); err != nil {
		return fmt.Errorf("enqueue persistent output: %w", err)
	}

	return nil
}

func (s *svc) prepareStoppedSessionInput(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	prompt string,
) (bool, error) {
	if record.Status != sessionstore.SessionStatusStopped {
		return false, nil
	}

	if !isReadOnlyBoundaryCommand(prompt) {
		if err := s.store.UpdateSessionStatus(
			ctx, record.ID, sessionstore.SessionStatusActive,
		); err != nil {
			return false, fmt.Errorf("resume stopped session %d: %w", record.ID, err)
		}

		return false, nil
	}

	commandOnly, err := s.commandOnlyStoppedRoot(ctx, record)
	if err != nil {
		return false, err
	}

	return !commandOnly, nil
}

func (s *svc) enqueueUserSessionInput(
	ctx context.Context,
	sessionID int64,
	prompt string,
) (*sessionstore.InboxInput, error) {
	attributes := make(map[string]any)
	switch strings.TrimSpace(prompt) {
	case "/schedules":
		content, err := s.schedulesCommand(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		attributes["schedules"] = content
	case "/status":
		current, err := s.CurrentProgress(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		attributes["status"] = current.Rendered
	}
	result, err := s.store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  sessionID,
			Source:     sessionstore.InputSourceUser,
			Content:    prompt,
			Attributes: attributes,
		},
	)
	if err != nil {
		return nil, err
	}
	return result.Input, nil
}

// loadModelCatalog records the configured models once: the subagent picker reads
// the names, SetModel reads the effort levels.
func (s *svc) loadModelCatalog(models []config.ModelEntry) {
	s.modelEntries = models

	for _, m := range models {
		s.modelCatalog = append(s.modelCatalog, subagent.ModelInfo{ID: m.ID, Name: m.Name, Tags: m.Tags})
	}
}

// checkModelConfigured guards the idle path, where no live session validates.
// An empty catalog means no config was loaded, so it vouches for nothing.
func (s *svc) checkModelConfigured(model string) error {
	if len(s.modelCatalog) == 0 {
		return nil
	}

	for _, m := range s.modelCatalog {
		if m.ID == model {
			return nil
		}
	}

	return fmt.Errorf("unknown model: %s", model)
}

func (s *svc) send(
	ctx context.Context,
	projectID int64,
	prompt, model string,
	attrs map[string]any,
) (int64, error) {
	workDir, err := s.store.GetProjectWorkDir(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("resolve project %d: %w", projectID, err)
	}

	if model == "" {
		model = s.defaultModel
	}

	level, err := s.resolveChildEffort(model, "", "")
	if err != nil {
		return 0, fmt.Errorf("resolve reasoning level for model %s: %w", model, err)
	}

	owner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	var rec *sessionstore.SessionRecord
	createdWithInput := false

	if owner != "" {
		projectName, nameErr := s.store.GetProjectName(ctx, projectID)
		if nameErr != nil {
			return 0, fmt.Errorf("resolve project name: %w", nameErr)
		}

		rec, _, err = s.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
			ProjectID: projectID, Model: model, ReasoningLevel: level, Attributes: attrs,
			Prompt: prompt, StartEpisode: prompt != "" && !isExactControlCommand(prompt),
			Name: projectName, WorkDir: workDir,
		})
		if err != nil {
			return 0, fmt.Errorf("create manager session record: %w", err)
		}

		createdWithInput = prompt != ""
	} else {
		rec, err = s.store.CreateSession(ctx, projectID, model, level, attrs)
		if err != nil {
			return 0, fmt.Errorf("create session record: %w", err)
		}
	}

	if prompt != "" && !createdWithInput {
		if _, err := s.store.Enqueue(
			ctx,
			sessionstore.Input{SessionID: rec.ID, Source: sessionstore.InputSourceUser, Content: prompt},
		); err != nil {
			return 0, fmt.Errorf("persist initial session input: %w", err)
		}
	}

	if err := s.ensureRunner(ctx, rec.ID, workDir, projectID); err != nil {
		if errors.Is(err, admission.ErrNoCapacity) {
			s.enqueuePendingRunner(rec.ID, workDir, projectID)
			return rec.ID, nil
		}

		// Cleanup: ensureRunner failed after the session/root was already committed.
		// Mark the session killed so it doesn't appear alive to managers, even though
		// the worktree (for /gwt failures) may already be gone. This prevents
		// orphaned sessions in the store that reference deleted directories.
		// WithoutCancel: the kill marker must land even if the request context
		// died mid-launch.
		if _, killErr := s.store.MarkSessionKilledWithOutput(
			context.WithoutCancel(ctx),
			rec.ID,
			0,
		); killErr != nil {
			logger.Ctx(ctx).Named("daemon.manager").Warn("cleanup_orphaned_session",
				zap.Int64("session_id", rec.ID), zap.Error(killErr))
		}

		return 0, err
	}

	return rec.ID, nil
}
