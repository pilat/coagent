package session

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func (s *Session) prepareHead(ctx context.Context, r *runState, batch *boundaryBatch) (bool, error) {
	if s.head == nil || r.headCommitted || !batch.accepted ||
		len(s.pendingInLoopCalls()) > 0 || s.HasPendingExternalCall() {
		return false, nil
	}

	head, err := s.head(ctx)
	if err != nil {
		return false, fmt.Errorf("render activation head: %w", err)
	}

	c := &batch.commit
	c.Messages = append([]*transcript.Message{hostUserMessage(head)}, c.Messages...)

	for i := range c.Accept {
		accept := &c.Accept[i]
		if accept.State == sessionstore.InputStateAccepted && accept.Content == "" && accept.LinkRef >= 0 {
			accept.LinkRef++
		}
	}

	c.State.ResetContext = true

	return true, nil
}
