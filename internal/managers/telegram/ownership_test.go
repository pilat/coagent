package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestManagerOwnsOnlyExplicitlyAttributedSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		attributes map[string]any
		wantOwned  bool
	}{
		{
			name: "already owned here",
			attributes: map[string]any{
				controllerapi.SessionAttributeManagerID: "telegram-main",
			},
			wantOwned: true,
		},
		{
			name: "owned by another manager",
			attributes: map[string]any{
				controllerapi.SessionAttributeManagerID: "telegram-secondary",
				"channel":                               "telegram",
			},
		},
		{
			name:       "channel alone is not proof of ownership",
			attributes: map[string]any{"channel": "telegram"},
		},
		{name: "ownerless without metadata"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := &Manager{id: "telegram-main"}
			assert.Equal(t, tt.wantOwned, manager.ownsSession(tt.attributes))
		})
	}
}
