package managerdelivery

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatRowText(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{"project · title", "**#7 project · title**\n\n**body**\nsecond line"},
		{"*project* · **title**", "**#7 project · title**\n\n**body**\nsecond line"},
		{"", "**#7 **\n\n**body**\nsecond line"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := ChatRow{Name: tt.name, Content: "**body**\nsecond line"}
			assert.Equal(t, tt.want, chatRowText(7, row))
			assert.Equal(t, tt.name, row.Name)
			assert.Equal(t, "**body**\nsecond line", row.Content)
		})
	}
}

func TestChatPrepareHeader(t *testing.T) {
	for _, tt := range []struct {
		name      string
		scheduled bool
		want      string
	}{
		{"project · title", false, "**→ #7 project · title**"},
		{"*project* · **title**", false, "**→ #7 project · title**"},
		{"*project* · **title**", true, "**→ #7 project · title ⏰**"},
		{"", false, "**→ #7**"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			fake := newChatFake()
			fake.sessions = []ChatSession{{ID: 7, Name: tt.name, Scheduled: tt.scheduled}}
			loop := NewChat(fake, fake).(*chatLoop)
			require.NoError(t, loop.prepare(t.Context(), 7))
			require.Len(t, loop.prelude, 1)
			assert.Equal(t, tt.want, loop.prelude[0].text)
			assert.Equal(t, tt.name, fake.sessions[0].Name)
		})
	}
}
