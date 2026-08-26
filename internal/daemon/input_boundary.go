package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type durableInputBoundary struct {
	store     sessionstore.InboxStore
	schedules schedule.Service
	sessionID int64
}

var _ session.InputBoundary = (*durableInputBoundary)(nil)

func (b *durableInputBoundary) Peek(ctx context.Context) (*session.PendingInput, error) {
	input, err := b.store.PeekPending(ctx, b.sessionID)
	if errors.Is(err, sessionstore.ErrNoPendingInput) {
		//nolint:nilnil // nil input is the InputBoundary EOF marker
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("peek session inbox: %w", err)
	}

	return &session.PendingInput{
		ID:         input.ID,
		Content:    input.RawContent,
		Attributes: input.Attributes,
		ReceivedAt: input.ReceivedAt,
	}, nil
}

func (b *durableInputBoundary) Accept(
	ctx context.Context,
	input session.PendingInput,
	prepared string,
	pendingCalls []session.PendingToolCall,
) (bool, bool, error) {
	for _, call := range pendingCalls {
		if call.Name != tool.IDSleep {
			return false, true, nil
		}
	}

	if len(pendingCalls) > 0 && b.schedules != nil {
		if _, err := b.schedules.CancelPendingSleeps(ctx, b.sessionID); err != nil {
			return false, false, fmt.Errorf("cancel interrupted sleep: %w", err)
		}
	}

	if _, err := b.store.PromoteInput(ctx, input.ID, prepared); err != nil {
		return false, false, fmt.Errorf("promote session input: %w", err)
	}

	return true, false, nil
}

func (b *durableInputBoundary) Reject(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.RejectInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("reject session input: %w", err)
	}

	return nil
}

func (b *durableInputBoundary) Handle(ctx context.Context, input session.PendingInput, reason string) error {
	if err := b.store.HandleInput(ctx, input.ID, reason); err != nil {
		return fmt.Errorf("handle session input: %w", err)
	}

	return nil
}

// HandleWithOutput keeps a session command's resolution and answer indivisible;
// narrow test stores retain the older Handle-only behavior.
func (b *durableInputBoundary) HandleWithOutput(
	ctx context.Context,
	input session.PendingInput,
	reason, content string,
) error {
	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); !owned {
		return b.Handle(ctx, input, reason)
	}

	outputs, ok := b.store.(sessionstore.CommandOutputStore)
	if !ok {
		return b.Handle(ctx, input, reason)
	}

	_, err := outputs.HandleInputWithOutput(ctx, input.ID, reason, sessionstore.OutputDraft{
		SessionID: b.sessionID,
		Type:      sessionstore.OutputMessagePersistent,
		Content:   content,
	})
	if err != nil {
		return fmt.Errorf("handle session input with output: %w", err)
	}

	return nil
}

// HandleSchedules resolves schedule display at the same FIFO boundary as text,
// so a later command cannot pass an earlier message.
func (b *durableInputBoundary) HandleSchedules(ctx context.Context, input session.PendingInput) (string, error) {
	content, err := b.renderSchedules(ctx)
	if err != nil {
		return "", err
	}

	if err := b.HandleWithOutput(ctx, input, "schedules command", content); err != nil {
		return "", err
	}

	return content, nil
}

func (b *durableInputBoundary) renderSchedules(ctx context.Context) (string, error) {
	if b.schedules == nil {
		return "No schedules for this session. Ask me to add one.", nil
	}

	entries, err := b.schedules.ListSchedules(ctx, b.sessionID)
	if err != nil {
		return "", fmt.Errorf("list schedules: %w", err)
	}

	if len(entries) == 0 {
		return "No schedules for this session. Ask me to add one.", nil
	}

	lines := []string{fmt.Sprintf("## Schedules (%d)", len(entries))}
	for _, entry := range entries {
		line := fmt.Sprintf("- #%d", entry.ID())
		switch {
		case entry.CronExpr() != "":
			cronExpr, timezone := schedule.SplitCronTZ(entry.CronExpr())
			line += fmt.Sprintf(" · cron `%s` (%s)", cronExpr, timezone)
		case entry.OneShotAt() != nil:
			line += " · once " + entry.OneShotAt().UTC().Format("2006-01-02 15:04 UTC")
		}

		if entry.Fresh() {
			line += " · fresh"
		}

		if prompt := strings.TrimSpace(entry.InputMessage()); prompt != "" {
			line += " · " + prompt
		}

		lines = append(lines, line)
	}

	lines = append(lines, "", "Ask me in chat to add, change, or remove a schedule.")

	return strings.Join(lines, "\n"), nil
}
