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
	desk := Desk{Focus: "devbox", TapeLastID: 38, Notes: []Note{
		{Key: "user.lang", Text: "Русский", WrittenAt: now.Add(-12 * 24 * time.Hour).Unix()},
		{Key: "rule.safe", Text: "safe"},
		{Key: "open.google-docs", Text: "OAuth"},
		{Key: "project.devbox", Text: "state"},
		{Key: "session.244", Text: "purpose", WrittenAt: now.Add(-2 * time.Hour).Unix()},
		{Key: "wait.244", Text: "retell", WrittenAt: now.Add(-time.Hour).Unix()},
		{Key: "session.999", Text: "gone"},
	}}
	projects := []Project{
		{
			Key:        "devbox",
			Place:      "repo ~/projects/devbox",
			Activity:   now.Add(-24 * time.Hour),
			SessionIDs: []int64{244},
			Sessions: []Card{
				{
					ID:            244,
					Project:       "devbox",
					Status:        "done",
					Activity:      now.Add(-24 * time.Hour),
					ContextTokens: 40000,
					CostUSD:       0.12,
					LastLine:      "ready",
					LastFinalAt:   now.Add(-time.Hour),
					Unread:        true,
				},
			},
		},
	}
	tape := []Line{{ID: 35, At: now, Kind: "say", Text: "say: «hello»"}}
	event := Event{LineID: 34, At: now, Kind: "observe", Text: "← devbox (244) answered: ready", Sessions: []int64{244}}
	got := Render(desk, projects, tape, event, now)
	want := "now: 2026-10-14 12:09 UTC\n\n" +
		"## you (11/1500)\nuser.lang (12d): Русский\nrule.safe: safe\n\n" +
		"## open (5/800)\nopen.google-docs: OAuth\n\n## focus\ndevbox\n\n" +
		"## catalog (projects by last activity; sessions you opened)\n" +
		"devbox  repo ~/projects/devbox  1d\n  project: state\n" +
		"  244  done  1d  ctx 40k  $0.12  [unread]\n    session (2h): purpose\n    last: \"ready\"\n    ⏳ wait (1h): retell\n" +
		"\n## orphaned\nsession.999: gone\n" +
		"\n## tape (visible lines 34–35 of 38)\n" +
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
