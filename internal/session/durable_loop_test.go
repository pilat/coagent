package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/transcript"
)

type firstReloadFailureStore struct {
	sessionstore.RuntimeStore
	err error
}

type compactionFailureStore struct{ sessionstore.RuntimeStore }

func (*compactionFailureStore) ReplaceCompactedMessages(
	context.Context,
	int64,
	[]int64,
	[]sessionstore.CompactionEntry,
) ([]int64, error) {
	return nil, errors.New("write conflict")
}

func (s *firstReloadFailureStore) LoadActiveMessages(ctx context.Context, id int64) ([]*transcript.Message, error) {
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, err
	}
	return s.RuntimeStore.LoadActiveMessages(ctx, id)
}

func runTestLoop(
	ctx context.Context,
	t *testing.T,
	agent *svc,
	opts loopOptions,
	cb iterationCallback,
) (*loopResult, error) {
	t.Helper()
	prepareDurableLoop(t, agent) //nolint:contextcheck // Fixture setup must survive the cancellation under test.
	return runLoop(ctx, agent, opts, cb)
}

func prepareDirectRun(t *testing.T, agent *svc, prompt string) {
	t.Helper()
	prepareDurableLoop(t, agent)
	if len(agent.ms.getMessages()) == 0 {
		if prompt == "" {
			prompt = noTaskPrompt
		}
		agent.boundary.(*loopInputBoundary).input = &PendingInput{ID: 1, Content: prompt}
	}
}

func prepareDurableLoop(t *testing.T, agent *svc) {
	t.Helper()
	if agent.dispositions != nil {
		return
	}
	if store, ok := agent.store.(sessionstore.ResponseDispositionStore); ok {
		agent.dispositions = store
	} else {
		_, store, sessionID := newFinalOutputStore(t)
		messages := agent.ms.getMessages()
		agent.store, agent.dispositions = store, store
		agent.id, agent.rootID = sessionID, sessionID
		agent.ms = newMessageStore(store, sessionID, store)
		for _, message := range messages {
			require.NoError(t, agent.ms.appendMessageLocked(context.Background(), &message))
		}
	}
	if agent.rootID == 0 {
		agent.rootID = agent.id
	}
	if agent.todoStore == nil {
		agent.todoStore = todo.New()
	}
	if agent.boundary == nil {
		agent.boundary = &loopInputBoundary{agent: agent}
	}
}

func (*receiptBoundary) HasBackgroundWakeSource(context.Context) (bool, error) { return false, nil }

func (*rejectingBoundary) HasBackgroundWakeSource(context.Context) (bool, error) { return false, nil }

func (*loopInputBoundary) HasBackgroundWakeSource(context.Context) (bool, error) {
	return false, nil
}
