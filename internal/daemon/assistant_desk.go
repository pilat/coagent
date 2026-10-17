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

// AssistantDesk renders persisted slots without running a model or marking cards read.
func (s *svc) AssistantDesk(ctx context.Context, id int64) (string, error) {
	record, err := s.store.GetSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load desk session: %w", err)
	}

	if record.AgentType != tool.AssistantAgentType {
		return "", errors.New("session has no assistant desk")
	}

	desk, err := s.assistantStore.LoadDesk(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load desk: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	sessions := &assistantSessions{daemon: s, root: id, owner: owner}

	catalog, err := sessions.Catalog(ctx, desk.LastTurnAt)
	if err != nil {
		return "", err
	}

	tape, err := s.assistantStore.VisibleTape(ctx, id, 40, 0)
	if err != nil {
		return "", fmt.Errorf("load desk tape: %w", err)
	}

	var event assistant.Event

	if len(tape) > 0 {
		last := tape[len(tape)-1]
		event = assistant.Event{LineID: last.ID, At: last.At, Kind: last.Kind, Text: last.Text}
	}

	return assistant.Render(desk, catalog, tape, event, time.Now().UTC()), nil
}
