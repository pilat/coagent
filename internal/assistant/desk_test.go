package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantRenderStatesTruncation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 14, 5, 0, 0, time.UTC)
	var focus []Focus
	for i := range 14 {
		focus = append(
			focus,
			Focus{ID: int64(i + 1), Text: fmt.Sprintf("item %d", i+1), UpdatedAt: now.Add(-2 * time.Hour)},
		)
	}
	var recent []Card
	for i := range 10 {
		recent = append(recent, Card{
			ID: int64(240 + i), Project: "devbox", Status: "done",
			CreatedAt: now.AddDate(0, 0, -14), Activity: now.Add(-2 * time.Hour),
		})
	}
	recent[0].Pending, recent[0].About, recent[0].ContextTokens = true, "Move the stand to podman", 53667
	text := Render(View{
		Desk: Desk{
			Memory: []Memory{
				{ID: 12, Text: "Short Russian answers."},
				{ID: 31, Text: "Searched кеш: nothing.", Until: "2026-10-25"},
			},
			Focus: focus,
			Conversation: []Line{
				{ID: 7, At: now.AddDate(0, 0, -1), Kind: userKind, Text: strings.Repeat("я", 2500)},
				{ID: 8, At: now, Kind: sayKind, Text: "готово"},
			},
			Spoken: 25,
		},
		Recent: recent, Total: 37,
	}, now)
	for _, want := range []string{
		"now: 2026-10-19 14:05 UTC\n",
		"## memory (44/4000)\n[m12] (permanent) Short Russian answers.\n[m31] (until 2026-10-25) Searched кеш: nothing.\n",
		"## focus (10 of 14 — focus_list shows all)\n[f1] 2h · item 1\n",
		"## recent sessions (10 of 37 — sessions lists all work, search finds older)\n",
		"240 devbox · done · created Oct 05 · touched 2h · ctx 54k · ⏳ awaiting answer\n    about: Move the stand to podman\n",
		"241 devbox · done · created Oct 05 · touched 2h\n",
		"## conversation (last 2 of 25 — search finds older)\nOct 18 14:05 user: «",
		"[…]\n14:05 you: «готово»\n",
	} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, text, "item 11")
	assert.NotContains(t, text, "## event")
	assert.NotContains(t, text, strings.Repeat("я", 2000))

	empty := Render(View{}, now)
	for _, want := range []string{"## focus (empty)", "## conversation (empty)", "## recent sessions (0 of 0"} {
		assert.Contains(t, empty, want)
	}
}

func TestAssistantMatchQueryDropsControlCharacters(t *testing.T) {
	t.Parallel()
	query, err := matchQuery("кеш\x00* x\ty")
	require.NoError(t, err)
	assert.Equal(t, `"кеш"* OR "x" OR "y"`, query)
}
