package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const budgetParkingNotice = "session %d is parking after a budget checkpoint — send the message again once it stops"

func (s *Service) sendInput(ctx context.Context, sessionID int64, prompt, key string) error {
	if s.parkRefuses(sessionID, prompt) {
		return fmt.Errorf(budgetParkingNotice, sessionID)
	}

	input, err := s.enqueueUserSessionInput(ctx, sessionID, prompt, key)
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		if record, getErr := s.store.GetSession(ctx, sessionID); getErr == nil && record.KilledAt != nil {
			return fmt.Errorf("session %d is killed", sessionID)
		}

		if s.parkRefuses(sessionID, prompt) {
			return fmt.Errorf(budgetParkingNotice, sessionID)
		}
	}

	if err != nil {
		return fmt.Errorf("persist session input: %w", err)
	}

	// A repeated key: the first send already took effect.
	if input == nil {
		return nil
	}

	if handled, err := s.handleGenericCommand(ctx, input); handled || err != nil {
		return err
	}

	unlock, err := s.lockSessionTree(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()

	return s.sendToSessionLocked(ctx, sessionID, prompt)
}

func (s *Service) sendToSessionLocked(ctx context.Context, sessionID int64, prompt string) error {
	if _, ok := s.runners.load(sessionID); ok {
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
	if err != nil || parked {
		return err
	}

	return s.startLocked(ctx, sessionID)
}

func (s *Service) SendToSessionResolved(ctx context.Context, sessionID int64, prompt, key string) (int64, error) {
	resolved, err := s.store.ResolveReplacement(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("resolve replacement session: %w", err)
	}

	sessionID = resolved

	if err := s.sendInput(ctx, sessionID, prompt, key); err != nil {
		return 0, err
	}

	return sessionID, nil
}

// SetModel keeps client construction and the persisted model choice consistent under the tree fence.
func (s *Service) SetModel(ctx context.Context, sessionID int64, model, reasoningLevel string) error {
	if err := s.models.configured(model); err != nil {
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

	if err := s.checkBudgetModel(ctx, sessionRootID(record), model); errors.Is(err, errUnpricedModel) {
		return errors.New("cannot switch an armed budget tree to a model without catalog pricing")
	} else if err != nil {
		return fmt.Errorf("load budget for model switch: %w", err)
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve model-switch project: %w", err)
	}

	in := s.build
	in.Record, in.WorkDir, in.Loader = record, workDir, nil

	client, section, err := sessionbuild.BuildClient(ctx, in, model, reasoningLevel)
	if err != nil {
		return fmt.Errorf("construct model %s: %w", model, err)
	}

	level := client.GetReasoningLevel()
	if err := s.store.UpdateSessionModel(ctx, sessionID, model, level); err != nil {
		_ = client.Close()
		return fmt.Errorf("update session model: %w", err)
	}

	rs, ok := s.runners.load(sessionID)
	if ok {
		if sess := rs.Service(); sess != nil {
			sess.SwitchModel(client, section)
			return nil
		}
	}

	_ = client.Close()

	return nil
}

// A runner exit also dispatches pending commands, so the second holder of the fence finds this one resolved.
func (s *Service) commandPending(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	rows, err := s.store.ListPending(ctx, input.SessionID)
	if err != nil {
		return false, fmt.Errorf("list pending commands: %w", err)
	}

	return slices.ContainsFunc(rows, func(row *sessionstore.InboxInput) bool { return row.ID == input.ID }), nil
}

func (s *Service) handleGenericCommand(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	if input.Source != sessionstore.InputSourceUser || !isEnqueueCommand(input.RawContent) {
		return false, nil
	}

	switch strings.TrimSpace(input.RawContent) {
	case statusCommand:
		unlock, err := s.lockSessionTree(ctx, input.SessionID)
		if err != nil {
			return true, err
		}
		defer unlock()

		return true, s.handleStatusInput(ctx, input)
	case stopCommand:
		return true, s.stop(ctx, input)
	case clearCommand:
		_, err := s.clear(ctx, input)
		return true, err
	case killCommand:
		return true, s.kill(ctx, input)
	default:
		return false, nil
	}
}

func (s *Service) handleStatusInput(ctx context.Context, input *sessionstore.InboxInput) error {
	current, err := s.progress.Current(ctx, input.SessionID)
	if err != nil {
		return fmt.Errorf("current progress: %w", err)
	}

	_, err = s.store.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "status command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{Type: sessionstore.OutputMessage, Content: current, MessageRef: -1},
		},
	})
	if errors.Is(err, sessionstore.ErrInputResolved) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("handle status input: %w", err)
	}

	s.publish(input.SessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: current})

	if !s.HasActiveLoop(input.SessionID) {
		s.publish(
			input.SessionID,
			sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle},
		)
	}

	return nil
}

func (s *Service) handleStoppedStop(ctx context.Context, input *sessionstore.InboxInput) error {
	_, err := s.store.Commit(ctx, sessionstore.Commit{
		SessionID: input.SessionID,
		Accept: []sessionstore.Accept{
			{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "stop command", LinkRef: -1},
		},
		Outputs: []sessionstore.Output{
			{
				Type:       sessionstore.OutputMessage,
				Content:    "Session already stopped.",
				Key:        fmt.Sprintf("input:%d:stop:already_stopped", input.ID),
				MessageRef: -1,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("handle stopped stop: %w", err)
	}

	return nil
}

func (s *Service) handleLifecycleInput(ctx context.Context, input *sessionstore.InboxInput, content string) error {
	command := strings.TrimPrefix(strings.TrimSpace(input.RawContent), "/")

	if !controllerapi.ManagerRoot(input.Attributes) {
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

func (s *Service) prepareStoppedSessionInput(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	prompt string,
) (bool, error) {
	if record.Status != sessionstore.SessionStatusStopped {
		return false, nil
	}

	if !isReadOnlyCommand(prompt) {
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

func (s *Service) enqueueUserSessionInput(
	ctx context.Context,
	sessionID int64,
	prompt, key string,
) (*sessionstore.InboxInput, error) {
	attributes, err := s.sessionInputAttributes(ctx, sessionID, prompt)
	if err != nil {
		return nil, err
	}

	if key != "" {
		key = "user:" + key
	}

	result, err := s.store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   sessionID,
			Source:      sessionstore.InputSourceUser,
			Content:     prompt,
			Attributes:  attributes,
			DeliveryKey: key,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("enqueue user session input: %w", err)
	}

	return result.Input, nil
}

func (s *Service) sessionInputAttributes(ctx context.Context, sessionID int64, prompt string) (map[string]any, error) {
	attributes := make(map[string]any)

	switch strings.TrimSpace(prompt) {
	case "/schedules":
		content, err := s.schedules.Render(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("render schedules: %w", err)
		}

		attributes["schedules"] = content
	case statusCommand:
		current, err := s.progress.Current(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("current progress: %w", err)
		}

		attributes["status"] = current
	}

	return attributes, nil
}
