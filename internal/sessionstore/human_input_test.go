package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsHumanInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source InputSource
		attrs  map[string]any
		want   bool
	}{
		{"user", InputSourceUser, nil, true},
		{"user attributes", InputSourceUser, map[string]any{"from": "telegram"}, true},
		{"observer integer", InputSourceUser, map[string]any{ObserverSessionIDAttribute: 1}, false},
		{"observer null", InputSourceUser, map[string]any{ObserverSessionIDAttribute: nil}, false},
		{"observer string", InputSourceUser, map[string]any{ObserverSessionIDAttribute: "invalid"}, false},
		{"assistant", InputSourceUser, map[string]any{"from": "assistant"}, false},
		{"agent", InputSourceAgent, nil, false},
		{"schedule", InputSourceSchedule, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsHumanInput(tc.source, tc.attrs))
		})
	}
}
