package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/tool"
)

// AssistantDesk renders the desk the next turn would see, without running a model.
func (s *svc) AssistantDesk(ctx context.Context, id int64) (string, error) {
	record, err := s.store.GetSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load desk session: %w", err)
	}

	if record.AgentType != tool.AssistantAgentType {
		return "", errors.New("session has no assistant desk")
	}

	tape, err := s.assistantStore.RecentTape(ctx, id, 1)
	if err != nil {
		return "", fmt.Errorf("load desk tape: %w", err)
	}

	var event assistant.Event

	if len(tape) > 0 {
		last := tape[len(tape)-1]
		event = assistant.Event{LineID: last.ID, Kind: last.Kind, Text: last.Text}
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	now := time.Now().UTC()

	desk, err := s.assistantStore.LoadDesk(ctx, id, event.LineID, now)
	if err != nil {
		return "", fmt.Errorf("load desk: %w", err)
	}

	sessions := &assistantSessions{daemon: s, root: id, owner: owner}

	recent, total, err := sessions.Recent(ctx, assistant.RecentSessions)
	if err != nil {
		return "", err
	}

	return assistant.Render(assistant.View{Desk: desk, Recent: recent, Total: total, Event: event}, now), nil
}
