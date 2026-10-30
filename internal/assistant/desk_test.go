package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestAssistantCardModelAndBudget(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, want string
		record     *budget.Record
	}{
		{"cost", "$1.20 left", &budget.Record{State: budget.Armed, CostLimitUSD: new(2.0), BaselineCostUSD: 10}},
		{"duration", "34m left", &budget.Record{State: budget.Armed, ArmedAt: now, DurationSeconds: new(int64(2040))}},
		{"hours", "1h05m left", &budget.Record{State: budget.Armed, ArmedAt: now, DurationSeconds: new(int64(3900))}},
		{"both", "$1.20 · 34m left", &budget.Record{
			State: budget.Armed, ArmedAt: now,
			CostLimitUSD: new(2.0), BaselineCostUSD: 10, DurationSeconds: new(int64(2040)),
		}},
		{"fired", "budget fired (cost)", &budget.Record{State: budget.Fired, FiredReason: "cost", ParkPhase: "requested"}},
		{"parked", "budget parked (duration)", &budget.Record{State: budget.Fired, FiredReason: "duration", ParkPhase: "parked"}},
		{"released", "", &budget.Record{State: budget.Released}},
		{"none", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			card := Card{
				ID: 7, Project: "project", Model: "model", Effort: "high", Budget: tc.record,
				TreeCostUSD: new(10.8), ContextTokens: 54000, Pending: true, Scheduled: true,
			}
			models := []Model{{ID: "model", DisplayName: "Display", EffortLevels: []string{"high"}}}
			line := cardLines(card, now, models)
			want := "ctx 54k · Display · high"
			if tc.want != "" {
				want += " · " + tc.want
			}
			assert.Contains(t, line, want+" · ⏰ · ⏳ awaiting answer")
			models[0].EffortLevels = nil
			assert.NotContains(t, cardLines(card, now, models), " · high")
			assert.Contains(t, cardLines(card, now, nil), "ctx 54k · model")
			assert.NotContains(t, cardLines(card, now, nil), " · high")
		})
	}
}

func TestAssistantCardEffectiveEffort(t *testing.T) {
	for _, tc := range []struct {
		name, stored, defaultEffort, want string
		levels, allowlist                 []string
	}{
		{"missing stored level", "", "high", "low", []string{"low", "high"}, []string{"low", "high"}},
		{"missing stored ignores model default", "", "max", "low", []string{"low", "high"}, []string{"low", "high"}},
		{"unsupported stored level", "medium", "high", "low", []string{"low", "high"}, []string{"low", "high"}},
		{"narrowed catalog", "high", "low", "low", []string{"low"}, []string{"low"}},
		{"supported stored level", "high", "low", "high", []string{"low", "high"}, []string{"low", "high"}},
		{"any effort", "max", "low", "max", []string{"low", "medium", "high"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			card := Card{ID: 7, Project: "project", Model: "model", Effort: tc.stored}
			models := []Model{
				{
					ID: "model", DisplayName: "Display", EffortLevels: tc.levels,
					EffortAllowlist: tc.allowlist, DefaultEffort: tc.defaultEffort,
				},
			}
			assert.Contains(t, cardLines(card, time.Now(), models), "Display · "+tc.want)
		})
	}
}

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
	recent[0].Pending, recent[0].Title, recent[0].ContextTokens = true, "Move the stand to podman", 53667
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
