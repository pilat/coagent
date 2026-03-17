package llm

import "encoding/json"

// reasoningEnvelope stamps a provider's reasoning payload with its producing model:
// replay is only legal back to that model, so provenance travels with the payload.
// Only this package opens it — everyone between here and the DB carries it sealed.
type reasoningEnvelope struct {
	Model   string          `json:"model"`
	Payload json.RawMessage `json:"payload"`
}

// wrapReasoning seals a payload for storage. An empty payload yields nil, so a
// model that returned no reasoning stores nothing.
func wrapReasoning(model string, payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return nil
	}

	sealed, err := json.Marshal(reasoningEnvelope{Model: model, Payload: payload})
	if err != nil {
		return nil
	}

	return sealed
}

// unwrapReasoning returns the payload only when it came from model; anything else
// reports false and the caller sends no reasoning for that message.
func unwrapReasoning(raw json.RawMessage, model string) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}

	var envelope reasoningEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}

	if envelope.Model != model || len(envelope.Payload) == 0 {
		return nil, false
	}

	return envelope.Payload, true
}
