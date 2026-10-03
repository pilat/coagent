package session

import (
	"context"
	"encoding/json"

	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *Session) persistState(ctx context.Context, iteration int, status sessionstore.SessionStatus) error {
	raw, err := json.Marshal(s.prompt.Todos.List())
	if err != nil {
		return err
	}
	data := json.RawMessage(raw)
	c := s.newCommit()
	c.State = sessionstore.StatePatch{Iteration: &iteration, Status: &status, TodoItems: &data}
	_, err = s.commit(ctx, c)
	return err
}
