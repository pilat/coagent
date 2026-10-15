package assistant

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestAssistantRender(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 14, 12, 9, 0, 0, time.UTC)
	desk := Desk{
		Diary:      map[string]string{"z": "я", "a": "b"},
		Stickers:   []Sticker{{Key: "wait", Text: "жду", On: "244"}, {Key: "note", Text: "x"}},
		Focus:      "devbox",
		TapeLastID: 38,
	}
	board := []Card{{
		ID: 244, Project: "devbox", Place: "repo ~/projects/devbox", Status: "done", Activity: now.Add(-24 * time.Hour),
		Asked: "check", LastLine: "ready", Unread: true,
	}}
	tape := []Line{{ID: 35, At: now, Kind: "say", Text: "say: «hello»"}}
	event := Event{LineID: 34, At: now, Kind: "observe", Text: "← devbox (244) answered: ready"}
	got := Render(desk, board, tape, event, now)
	want := "now: 2026-10-14 12:09 UTC\n\n" +
		"## notebook (2/30 entries, 4/2000 chars)\na: b\nz: я\n\n" +
		"## stickers (2/8, 4/1000 chars)\n- wait @244: жду\n- note: x\n\n## focus\ndevbox\n\n## board\n" +
		"244  devbox  repo ~/projects/devbox  done  Oct 13 12:09  asked: \"check\"  last: \"ready\"  [unread]\n\n" +
		"## tape (visible lines 34–35 of 38)\n" +
		"34 12:09 ← devbox (244) answered: ready\n35 12:09 say: «hello»\n\n" +
		"## event (line 34)\n← devbox (244) answered: ready\n\n" +
		"Answer with exactly one JSON object and nothing else.\n"
	assert.Equal(t, want, got)
	assert.Len(t, tape, 1)
}

func TestAssistantRenderBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 14, 12, 9, 0, 0, time.UTC)
	text := strings.Repeat("я", 20000) + "\nTHE END"
	got := Render(Desk{}, nil, nil, Event{LineID: 1, At: now, Text: text}, now)
	_, event, _ := strings.Cut(got, "## event (line 1)\n")
	body, _, _ := strings.Cut(event, "\n\nAnswer")
	assert.Equal(t, 20000, utf8.RuneCountInString(body))
	assert.Contains(t, body, "\n[…]\n")
	assert.True(t, strings.HasSuffix(body, "THE END"), "the tail of a long answer stays visible")
	assert.NotContains(t, got, "�")
	assert.Equal(t, "short", ClipMiddle("short", 10))
}

func TestAssistantTapeRenderBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 14, 12, 9, 0, 0, time.UTC)
	for _, size := range []int{5999, 6000, 6001} {
		text := strings.Repeat("я", size)
		tape := []Line{{ID: 1, At: now, Kind: "user", Text: text}}
		var rendered strings.Builder
		renderTape(&rendered, 1, tape, Event{}, now)
		_, body, _ := strings.Cut(rendered.String(), "1 12:09 ")
		body = strings.TrimSuffix(body, "\n")
		assert.Equal(t, min(size, 6000), utf8.RuneCountInString(body))
		if size > 6000 {
			assert.Equal(t, strings.Repeat("я", 5997)+"[…]", body)
		} else {
			assert.Equal(t, text, body)
		}

		assert.Equal(t, text, tape[0].Text)
	}
}
