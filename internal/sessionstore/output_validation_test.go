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
			draft:   OutputDraft{Type: OutputMessage, Content: "x"},
			message: "invalid output draft",
		},
		{name: "type", draft: OutputDraft{SessionID: 1, Type: "unknown"}, message: "invalid output draft"},
		{name: "blank source key", draft: OutputDraft{
			SessionID: 1, Type: OutputMessage, Content: "x", SourceKey: " ", Fingerprint: "digest",
		}, message: "empty output identity"},
		{name: "blank fingerprint", draft: OutputDraft{
			SessionID: 1, Type: OutputMessage, Content: "x", SourceKey: "key", Fingerprint: " ",
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
		require.NoError(t, validateProducerAttributes(OutputMessage, map[string]any{"source": source}))
	}

	require.NoError(t, validateProducerAttributes(OutputMessage, nil))
	require.Error(t, validateProducerAttributes(OutputMessage, map[string]any{"source": "person"}))
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

func TestOutputMessage_UsesOnlySourceInProducerAttributesAndFingerprint(t *testing.T) {
	attributes := map[string]any{"source": outputSourceAgent}
	want := OutputFingerprint(OutputMessage, "answer", 7, attributes)
	for _, key := range []string{"waiting", "waiting_identity", "progress_revision", "message_ids", ModelInputGenerationAttribute} {
		attributes[key] = "old metadata"
		assert.Equal(t, want, OutputFingerprint(OutputMessage, "answer", 7, attributes))
		require.ErrorContains(t, validateProducerAttributes(OutputMessage, attributes), key)
		delete(attributes, key)
	}
}
