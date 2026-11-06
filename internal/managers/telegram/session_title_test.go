package telegram

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTitleCommand(t *testing.T) {
	for _, tt := range []struct {
		name, input, reply, title string
		focus                     int64
	}{
		{"rename", "/title fix login 500", "Renamed #12: fix login 500", "fix login 500", 12},
		{"trim", "/title   fix login 500  ", "Renamed #12: fix login 500", "fix login 500", 12},
		{"usage", "/title", "Usage: /title text.", "", 12},
		{"long", "/title " + strings.Repeat("界", 81), "Title must be one line of 1-80 characters.", "", 12},
		{"multiline", "/title two\nlines", "Title must be one line of 1-80 characters.", "", 12},
		{"bot suffix", "/title@bot x", "Renamed #12: x", "x", 12},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var messages []string
			ctrl := &fakeController{focus: tt.focus}
			m := newCmdTestManager(t, ctrl, &messages)
			m.handleChatText(t.Context(), normalizeTextCommand(tt.input))
			assert.Equal(t, []string{tt.reply}, messages)
			assert.Empty(t, ctrl.messageCalls)
			if tt.title == "" {
				assert.Empty(t, ctrl.titleCalls)
				return
			}
			require.Len(t, ctrl.titleCalls, 1)
			assert.Equal(t, tt.focus, ctrl.titleCalls[0].id)
			assert.Equal(t, tt.title, ctrl.titleCalls[0].title)
		})
	}
}
