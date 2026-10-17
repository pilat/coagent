package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCatalogCutsAndAlwaysShown(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000000, 0)
	project := Project{Key: "devbox", Place: "repo /devbox"}
	for id := int64(1); id <= 20; id++ {
		project.Sessions = append(
			project.Sessions,
			Card{ID: id, Activity: now.Add(time.Duration(id) * time.Minute), Unread: id == 1, Pending: id == 4},
		)
		project.SessionIDs = append(project.SessionIDs, id)
	}
	desk := Desk{Notes: []Note{{Key: "wait.3", Text: "retell"}}}
	event := Event{Sessions: []int64{2}}
	shown := TrimCatalog([]Project{project}, desk, event, now)
	require.Len(t, shown.Cards, 9)
	for _, id := range []int64{1, 2, 3, 4, 16, 17, 18, 19, 20} {
		assert.Contains(t, shown.Cards, cardWithID(shown.Cards, id))
	}
	assert.Contains(t, Render(desk, []Project{project}, nil, event, now), "+11 older sessions")
	a := Answer{Do: []Action{{Read: &ReadAction{Session: 5, Lines: 1}}}}
	assert.Contains(
		t,
		violationsText(Validate(a, desk, DefaultRules(), ValidationContext{Shown: shown})),
		"session 5 is not in the catalog",
	)
}

func TestCatalogBudgetAndPinnedProject(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000000, 0)
	var projects []Project
	desk := Desk{}
	for index := range 60 {
		key := fmt.Sprintf("project-%02d", index)
		projects = append(
			projects,
			Project{
				Key:        key,
				Place:      "repo /" + key,
				SessionIDs: []int64{int64(index + 1)},
				Sessions: []Card{
					{ID: int64(index + 1), Asked: strings.Repeat("я", 120), LastLine: strings.Repeat("ж", 200)},
				},
			},
		)
		desk.Notes = append(desk.Notes, Note{Key: "project." + key, Text: strings.Repeat("z", 300)})
	}
	desk.Notes = append(desk.Notes, Note{Key: "wait.60", Text: "pinned"})
	shown := TrimCatalog(projects, desk, Event{}, now)
	require.NotEmpty(t, shown.Projects)
	assert.Equal(t, "project-59", shown.Projects[0].Key)
	assert.Positive(t, shown.MoreProjects)
	rendered := Render(desk, projects, nil, Event{}, now)
	_, section, _ := strings.Cut(rendered, catalogHeader)
	section, _, _ = strings.Cut(section, "\n## tape")
	assert.LessOrEqual(t, utf8.RuneCountInString(catalogHeader+section), catalogBudget)
	assert.NotContains(t, rendered, "## orphaned", "notes on cut sessions and projects still resolve")
	ctx := ValidationContext{OriginKind: "user", Shown: shown, ProjectKeys: shown.ProjectKeys}
	a := Answer{Notes: map[string]*string{"project.project-58": new("updated")}}
	assert.Empty(t, Validate(a, desk.Edited(a, now), DefaultRules(), ctx))
}

func TestCatalogFallbackAndNewerFinal(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000000, 0)
	projects := []Project{
		{
			Key:        "p",
			Sessions:   []Card{{ID: 1, Asked: "original", LastLine: "final", LastFinalAt: now}},
			SessionIDs: []int64{1},
		},
	}
	got := Render(Desk{}, projects, nil, Event{}, now)
	assert.Contains(t, got, "asked: \"original\"")
	assert.Contains(t, got, "last: \"final\"")
	desk := Desk{Notes: []Note{{Key: "session.1", Text: "recap", WrittenAt: now.Unix()}}}
	got = Render(desk, projects, nil, Event{}, now)
	assert.Contains(t, got, "session (now): recap")
	assert.NotContains(t, got, "last:")
	assert.NotContains(t, got, "asked:")
	projects[0].Sessions[0].LastFinalAt = now.Add(time.Second)
	assert.Contains(t, Render(desk, projects, nil, Event{}, now), "last: \"final\"")
}

func TestCatalogCollapsedHeadAndOrphans(t *testing.T) {
	t.Parallel()
	desk := Desk{Notes: []Note{
		{Key: "project.empty", Text: strings.Repeat("я", 300)},
		{Key: "project.gone", Text: "missing"},
		{Key: "wait.raw", Text: "running"},
		{Key: "session.9", Text: "killed"},
	}}
	got := Render(desk, []Project{{Key: "empty", Place: "scratch"}}, nil, Event{}, time.Unix(1000, 0))
	assert.Contains(t, got, "project: "+strings.Repeat("я", 117)+"[…]")
	assert.Contains(t, got, "## orphaned\nproject.gone: missing\nsession.9: killed\nwait.raw: running")
}

func cardWithID(cards []Card, id int64) Card {
	for _, card := range cards {
		if card.ID == id {
			return card
		}
	}
	return Card{ID: -1}
}

func TestCatalogMandatorySessionsCanExceedBudget(t *testing.T) {
	t.Parallel()
	var projects []Project
	desk := Desk{}
	for index := range 20 {
		id := int64(index + 1)
		key := fmt.Sprintf("p%d", index)
		projects = append(
			projects,
			Project{Key: key, Sessions: []Card{{ID: id, Unread: true}}, SessionIDs: []int64{id}},
		)
		desk.Notes = append(desk.Notes, Note{Key: fmt.Sprintf("session.%d", id), Text: strings.Repeat("я", 300)})
	}
	shown := TrimCatalog(projects, desk, Event{}, time.Now())
	assert.Len(t, shown.Cards, 20)
	assert.Zero(t, shown.MoreProjects)
}

func TestCatalogOmitsZeroSignals(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000000, 0)
	project := Project{Key: "devbox", Place: "repo /devbox", SessionIDs: []int64{1, 2, 3}}
	project.Sessions = []Card{
		{ID: 1, Status: "done", Activity: now},
		{ID: 2, Status: "done", Activity: now, ContextTokens: 40000},
		{ID: 3, Status: "done", Activity: now, CostUSD: 0.12},
	}
	out := Render(Desk{}, []Project{project}, nil, Event{}, now)
	assert.NotContains(t, out, "ctx 0")
	assert.NotContains(t, out, "$0.00")
	assert.Contains(t, out, "ctx 40k")
	assert.Contains(t, out, "$0.12")
	assert.NotContains(t, out, "  1  done  ctx")
	assert.NotContains(t, out, "  1  done  $")
}
