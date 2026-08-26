package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOutputDraft_RejectsEachInvalidIdentityComponent(t *testing.T) {
	tests := []struct {
		name    string
		draft   OutputDraft
		message string
	}{
		{
			name:    "session",
			draft:   OutputDraft{Type: OutputMessagePersistent, Content: "x"},
			message: "invalid output draft",
		},
		{name: "type", draft: OutputDraft{SessionID: 1, Type: "unknown"}, message: "invalid output draft"},
		{name: "blank source key", draft: OutputDraft{
			SessionID: 1, Type: OutputMessagePersistent, Content: "x", SourceKey: " ", Fingerprint: "digest",
		}, message: "empty output identity"},
		{name: "blank fingerprint", draft: OutputDraft{
			SessionID: 1, Type: OutputMessagePersistent, Content: "x", SourceKey: "key", Fingerprint: " ",
		}, message: "empty output identity"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.EqualError(t, validateOutputDraft(tt.draft), tt.message)
		})
	}
}

func TestValidateProducerAttributes_MessageSourceVariants(t *testing.T) {
	for _, source := range []string{outputSourceScheduler, outputSourceAgent} {
		require.NoError(t, validateProducerAttributes(OutputMessagePersistent, map[string]any{"source": source}))
	}

	require.NoError(t, validateProducerAttributes(OutputMessagePersistent, nil))
	require.Error(t, validateProducerAttributes(OutputMessagePersistent, map[string]any{"source": "person"}))
}

func TestValidateWaitingPayload_RejectsEachBrokenPairInvariant(t *testing.T) {
	validSleep := []map[string]any{{"wake_at": "2026-08-26T19:00:00Z"}}
	validSleepIdentity := []map[string]any{{"tool_call_id": "call-1"}}
	validChild := []map[string]any{{"child_id": int64(7)}}
	validChildIdentity := []map[string]any{{"child_id": int64(7), "activation_seq": int64(2)}}

	require.NoError(t, validateWaitingPayload(validSleep, validSleepIdentity))
	require.NoError(t, validateWaitingPayload(validChild, validChildIdentity))

	tests := []struct {
		name     string
		display  any
		identity any
	}{
		{name: "empty", display: []map[string]any{}, identity: []map[string]any{}},
		{name: "different lengths", display: validSleep, identity: []map[string]any{}},
		{name: "empty wake time", display: []map[string]any{{"wake_at": ""}}, identity: validSleepIdentity},
		{
			name:     "sleep display has extra field",
			display:  []map[string]any{{"wake_at": "x", "extra": true}},
			identity: validSleepIdentity,
		},
		{
			name:     "sleep identity has extra field",
			display:  validSleep,
			identity: []map[string]any{{"tool_call_id": "call-1", "extra": true}},
		},
		{name: "sleep identity is empty", display: validSleep, identity: []map[string]any{{"tool_call_id": ""}}},
		{name: "child id absent", display: []map[string]any{{"other": int64(7)}}, identity: validChildIdentity},
		{
			name:     "child id differs",
			display:  validChild,
			identity: []map[string]any{{"child_id": int64(8), "activation_seq": int64(2)}},
		},
		{
			name:     "identity child absent",
			display:  validChild,
			identity: []map[string]any{{"other": int64(7), "activation_seq": int64(2)}},
		},
		{
			name:     "activation absent",
			display:  validChild,
			identity: []map[string]any{{"child_id": int64(7), "other": int64(2)}},
		},
		{
			name:     "child display has extra field",
			display:  []map[string]any{{"child_id": int64(7), "extra": true}},
			identity: validChildIdentity,
		},
		{
			name:     "child identity has extra field",
			display:  validChild,
			identity: []map[string]any{{"child_id": int64(7), "activation_seq": int64(2), "extra": true}},
		},
		{name: "sleep does not skip later invalid child", display: []map[string]any{
			{"wake_at": "2026-08-26T19:00:00Z"}, {"child_id": int64(7)},
		}, identity: []map[string]any{
			{"tool_call_id": "call-1"}, {"child_id": int64(7), "other": int64(2)},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, validateWaitingPayload(tt.display, tt.identity))
		})
	}
}

func TestPositiveInt64_RequiresPositiveIntegralValues(t *testing.T) {
	for _, value := range []any{int64(1), int(1), float64(1)} {
		actual, ok := positiveInt64(value)
		assert.True(t, ok)
		assert.Equal(t, int64(1), actual)
	}

	for _, value := range []any{int64(0), int(0), int(-1), float64(0), float64(-1), 1.5, "1"} {
		_, ok := positiveInt64(value)
		assert.False(t, ok)
	}
}
