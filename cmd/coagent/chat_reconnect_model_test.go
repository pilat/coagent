package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/managers/cli"
)

type secretModel struct {
	requested map[string]bool
	answered  map[string]bool
	dismissed map[string]bool
	replayed  map[string]bool
}

func newSecretModel() *secretModel {
	return &secretModel{
		requested: make(map[string]bool),
		answered:  make(map[string]bool),
		dismissed: make(map[string]bool),
		replayed:  make(map[string]bool),
	}
}

func (m *secretModel) request(id string) bool {
	if m.requested[id] {
		m.replayed[id] = true

		return false
	}

	m.requested[id] = true

	return true
}

func (m *secretModel) answer(id string) { m.answered[id] = true }

func (m *secretModel) failAnswer(id string) bool {
	delete(m.answered, id)

	replayed := m.replayed[id]
	delete(m.replayed, id)
	if !replayed {
		delete(m.requested, id)
	}

	return replayed
}

func (m *secretModel) resolve(id string) {
	if m.answered[id] {
		delete(m.answered, id)
		delete(m.requested, id)
		delete(m.replayed, id)

		return
	}

	m.dismissed[id] = true
	delete(m.replayed, id)
}

func (m *secretModel) claimDismissed(id string) bool {
	if !m.dismissed[id] {
		return false
	}

	delete(m.dismissed, id)
	delete(m.requested, id)
	delete(m.replayed, id)

	return true
}

// A failed answer gives up its echo claim, so a replay can be dismissed by the
// terminal that actually resolves it.
func TestHarnessModel_SecretReplayIsIdempotentAcrossFailedAnswers(t *testing.T) {
	t.Parallel()

	const requestID = "req-restart"

	model := newSecretModel()
	actual := newChat("unused", newScriptedTerminal(0), &syncBuffer{}, &syncBuffer{})
	req := cli.SecretRequest{RequestID: requestID, Name: "OPENAI_API_KEY"}

	assert.Equal(t, model.request(requestID), actual.acceptSecret(req))
	assert.Equal(t, model.request(requestID), actual.acceptSecret(req), "duplicate replay")

	model.answer(requestID)
	actual.noteAnswered(requestID)
	assert.True(t, model.failAnswer(requestID), "the model retained the suppressed replay")
	actual.retryAnswer(requestID)
	retried := <-actual.secrets
	assert.Equal(t, req, retried, "production requeued the suppressed replay")

	model.resolve(requestID)
	resolved, err := json.Marshal(cli.SecretResolved{RequestID: requestID, Name: "OPENAI_API_KEY"})
	require.NoError(t, err)
	actual.dismissSecret(resolved)

	assert.Equal(t, model.claimDismissed(requestID), actual.claimDismissed(requestID))
	assert.Equal(t, model.request(requestID), actual.acceptSecret(req), "resolved request releases its ID")
}
