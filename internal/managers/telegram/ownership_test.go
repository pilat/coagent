package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestManagerOwnsOnlyExplicitlyAttributedSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		owner     string
		wantOwned bool
	}{
		{
			name:      "already owned here",
			owner:     "telegram-main",
			wantOwned: true,
		},
		{
			name:  "owned by another manager",
			owner: "telegram-secondary",
		},
		{
			name:  "channel alone is not proof of ownership",
			owner: "",
		},
		{name: "ownerless without metadata"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := &Manager{id: "telegram-main"}
			assert.Equal(t, tt.wantOwned, manager.ownsSession(tt.owner))
		})
	}
}
