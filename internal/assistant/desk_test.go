package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
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
				{ID: 7, At: now.AddDate(0, 0, -1), Kind: userKind, Text: "user: «" + strings.Repeat("я", 2500) + "»"},
				{ID: 8, At: now, Kind: sayKind, Text: "say: «готово»"},
			},
			Spoken: 25,
		},
		Recent: recent, Total: 37,
		Event: Event{Text: strings.Repeat("a", eventTextLimit) + "tail"},
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
		"## event\n", "\n[…]\n", "tail\n",
	} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, text, "item 11")
	assert.NotContains(t, text, strings.Repeat("я", 2000))

	empty := Render(View{}, now)
	for _, want := range []string{"## focus (empty)", "## conversation (empty)", "## recent sessions (0 of 0"} {
		assert.Contains(t, empty, want)
	}
}

func TestAssistantToolValidation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	full := []Memory{{ID: 1, Text: strings.Repeat("x", memoryTotalChars)}}
	for _, tc := range []struct {
		name   string
		tool   string
		args   string
		memory []Memory
		focus  int
		want   string
	}{
		{"open without about", toolOpen, `{"project":"devbox","text":"go"}`, nil, 0, "about must be one line"},
		{"open about lines", toolOpen, `{"project":"devbox","about":"a\nb","text":"go"}`, nil, 0, "about must be one line"},
		{
			"open long about", toolOpen, `{"project":"devbox","about":"` + strings.Repeat("a", 201) + `","text":"go"}`, nil, 0,
			"about must be one line",
		},
		{"open without text", toolOpen, `{"project":"devbox","about":"why"}`, nil, 0, "needs a project and a text"},
		{"open bad project", toolOpen, `{"project":"bad","about":"why","text":"go"}`, nil, 0, "bad project"},
		{"send unknown session", toolSend, `{"session":999,"text":"go"}`, nil, 0, "not a work session"},
		{"send stray handle", toolSend, `{"session":"new-7-1","text":"go"}`, nil, 0, "nor a handle opened in this turn"},
		{"send without text", toolSend, `{"session":244,"text":" "}`, nil, 0, "send needs a text"},
		{"stop handle", toolStop, `{"session":"new-1-1"}`, nil, 0, "neither a session id"},
		{"read count", toolRead, `{"session":244,"count":61}`, nil, 0, "count must be between 1 and 60"},
		{"read unknown", toolRead, `{"session":5,"count":1}`, nil, 0, "not a work session"},
		{"around count", toolAround, `{"ref":"msg 1","count":11}`, nil, 0, "count must be between 1 and 10"},
		{"remember empty", toolRemember, `{"text":" "}`, nil, 0, "needs text"},
		{"remember long", toolRemember, `{"text":"` + strings.Repeat("ж", 301) + `"}`, nil, 0, "at most 300 characters"},
		{"remember bad date", toolRemember, `{"text":"x","until":"next week"}`, nil, 0, "must be a date"},
		{"remember full", toolRemember, `{"text":"x"}`, full, 0, "memory is full (4000/4000)"},
		{"rewrite unknown", toolRewriteMemory, `{"id":"m9","text":"x"}`, nil, 0, `no memory note "m9"`},
		{"forget unknown", toolForgetMemory, `{"id":"m-new-1"}`, nil, 0, "no memory note"},
		{"focus empty", toolFocus, `{"text":"","sessions":[]}`, nil, 0, "one line of text"},
		{"focus long", toolFocus, `{"text":"` + strings.Repeat("f", 201) + `","sessions":[]}`, nil, 0, "at most 200"},
		{"focus bad ref", toolFocus, `{"text":"x","sessions":["devbox"]}`, nil, 0, "neither a session id"},
		{"focus unknown session", toolFocus, `{"text":"x","sessions":[999]}`, nil, 0, "not a work session"},
		{"focus full", toolFocus, `{"text":"x","sessions":[]}`, nil, focusLiveItems, "at most 30 items"},
		{"update unknown", toolUpdateFocus, `{"id":"f9","text":"x","sessions":[]}`, nil, 0, `no focus item "f9"`},
		{"done unknown", toolDoneFocus, `{"id":"f9"}`, nil, 0, `no focus item "f9"`},
		{"bad arguments", toolSend, `{"session":true}`, nil, 0, "arguments for send"},
		{"unknown tool", "browse", `{}`, nil, 0, `unknown tool "browse"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var focus []Focus
			for i := range tc.focus {
				focus = append(focus, Focus{ID: int64(i + 1), Text: "busy"})
			}
			l, tr := toolHarness(tc.memory, focus, now)
			_, err := l.runTool(t.Context(), tr, use(tc.tool, tc.args), phaseMain)
			require.ErrorContains(t, err, tc.want)
			assert.Empty(t, tr.effects)
		})
	}
}

func TestAssistantMemoryLimitAllowsShrinking(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	over := []Memory{{ID: 1, Text: strings.Repeat("x", 290)}, {ID: 2, Text: strings.Repeat("y", memoryTotalChars)}}
	l, tr := toolHarness(over, nil, now)
	_, err := l.runTool(t.Context(), tr, use(toolRewriteMemory, `{"id":"m1","text":"short"}`), phaseMain)
	require.NoError(t, err, "an edit that shrinks over-limit memory always passes")
	_, err = l.runTool(t.Context(), tr, use(toolRewriteMemory, `{"id":"1","text":"`+strings.Repeat("z", 20)+`"}`),
		phaseMain)
	require.ErrorContains(t, err, "memory is full")
	assert.Equal(t, []string{"memory is full (4005/4000): merge, shorten or remove notes"}, tr.problems())
	_, err = l.runTool(t.Context(), tr, use(toolForgetMemory, `{"id":"m2"}`), phaseMain)
	require.NoError(t, err)
	assert.Empty(t, tr.problems())
	assert.Equal(t, 5, tr.memoryChars())
}

func TestAssistantFocusListShowsTheBuffer(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	l, tr := toolHarness(
		nil,
		[]Focus{{ID: 3, Text: "old", Sessions: []string{"244"}, UpdatedAt: now.Add(-3 * time.Hour)}},
		now,
	)
	_, err := l.runTool(t.Context(), tr, use(toolFocus, `{"text":"new item","sessions":[244]}`), phaseMain)
	require.NoError(t, err)
	_, err = l.runTool(t.Context(), tr, use(toolDoneFocus, `{"id":"3"}`), phaseMain)
	require.NoError(t, err)
	text, err := l.runTool(t.Context(), tr, use(toolFocusList, `{}`), phaseMain)
	require.NoError(t, err)
	assert.Equal(t, "[f-new-1] now · new item → 244\n", text)
}

func toolHarness(memory []Memory, focus []Focus, now time.Time) (*loop, *turn) {
	l := &loop{deps: Deps{
		Sessions: &loopSessions{cards: []Card{{ID: 244, Project: "devbox", Status: "done"}}},
		Now:      func() time.Time { return now },
	}}
	tr := newTurn(&sessionstore.InboxInput{ID: 1}, memory, focus, now)
	tr.messages = []llmwire.Message{{Role: llmwire.RoleUser, Content: "desk"}}

	return l, tr
}

func TestAssistantUpdateFocusKeepsOmittedSessions(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	l, tr := toolHarness(nil, []Focus{{ID: 3, Text: "wait", Sessions: []string{"244"}}}, now)
	_, err := l.runTool(t.Context(), tr, use(toolUpdateFocus, `{"id":"f3","text":"still waiting"}`), phaseMain)
	require.NoError(t, err)
	assert.Equal(t, []string{"244"}, tr.findFocus("f3").Sessions)
	_, err = l.runTool(t.Context(), tr, use(toolUpdateFocus, `{"id":"f3","text":"no wait","sessions":[]}`), phaseMain)
	require.NoError(t, err)
	assert.Empty(t, tr.findFocus("f3").Sessions, "an explicit empty list clears the refs")
}

func TestAssistantKeepNormalizesReplay(t *testing.T) {
	t.Parallel()
	_, tr := toolHarness(nil, nil, time.Now().UTC())
	keep(tr, &llmwire.Response{Text: " \n", ToolCalls: []llmwire.ToolCall{{Name: toolFocusList}}}, true)
	replayed := tr.messages[len(tr.messages)-1]
	assert.Empty(t, replayed.Content, "whitespace-only text is never replayed as a text block")
	assert.NotEmpty(t, replayed.ToolCalls[0].ID, "a call without an id gets one its result can answer")
	assert.Empty(t, tr.replies)
}

func TestAssistantMatchQueryDropsControlCharacters(t *testing.T) {
	t.Parallel()
	query, err := matchQuery("кеш\x00* x\ty")
	require.NoError(t, err)
	assert.Equal(t, `"кеш"* OR "x" OR "y"`, query)
}
