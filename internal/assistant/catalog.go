package assistant

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	catalogSessionsPerProject = 5
	catalogBudget             = 5000
	catalogHeader             = "## catalog (projects by last activity; sessions you opened)\n"
)

// Project carries full session identities even when only candidate cards are enriched.
type Project struct {
	ProjectID  int64
	Name       string
	Key        string
	Place      string
	Activity   time.Time
	Sessions   []Card
	SessionIDs []int64
}

// ShownCatalog is the shared projection used for rendering and action authority.
type ShownCatalog struct {
	Projects     []Project
	Cards        []Card
	MoreProjects int
	ProjectKeys  map[string]bool
	SessionIDs   map[int64]bool
}

// SelectSessions selects metadata before the daemon loads requests, finals and costs.
func SelectSessions(cards []Card, notes []Note, eventIDs []int64) []Card {
	ordered := slices.Clone(cards)
	slices.SortFunc(ordered, func(a, b Card) int {
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		if a.ID > b.ID {
			return -1
		}

		if a.ID < b.ID {
			return 1
		}

		return 0
	})

	indexed := noteMap(notes)
	var selected []Card

	for index, card := range ordered {
		if index < catalogSessionsPerProject || mandatoryCard(card, indexed, eventIDs) {
			selected = append(selected, card)
		}
	}

	return selected
}

// TrimCatalog applies one budget and retains every mandatory session ahead of it.
func TrimCatalog(projects []Project, d Desk, event Event, now time.Time) ShownCatalog {
	shown := ShownCatalog{ProjectKeys: map[string]bool{}, SessionIDs: map[int64]bool{}}
	notes := noteMap(d.Notes)
	groups := [3][]Project{}

	for _, project := range projects {
		shown.ProjectKeys[project.Key] = true
		for _, id := range project.SessionIDs {
			shown.SessionIDs[id] = true
		}

		for _, card := range project.Sessions {
			shown.SessionIDs[card.ID] = true
		}

		project.Sessions = SelectSessions(project.Sessions, d.Notes, event.Sessions)

		group := 2
		if len(project.Sessions) > 0 {
			group = 1
		}

		if slices.ContainsFunc(
			project.Sessions,
			func(card Card) bool { return mandatoryCard(card, notes, event.Sessions) },
		) {
			group = 0
		}

		groups[group] = append(groups[group], project)
	}

	used := utf8.RuneCountInString(catalogHeader)

	for group, entries := range groups {
		for _, project := range entries {
			text := renderProject(project, notes, now)
			size := utf8.RuneCountInString(text)

			more := fmt.Sprintf("+%d more projects\n", len(projects))
			if group != 0 && used+size+utf8.RuneCountInString(more) > catalogBudget {
				shown.MoreProjects++
				continue
			}

			used += size

			shown.Projects = append(shown.Projects, project)
			shown.Cards = append(shown.Cards, project.Sessions...)
		}
	}

	return shown
}

func mandatoryCard(card Card, notes map[string]Note, eventIDs []int64) bool {
	_, waiting := notes["wait."+strconv.FormatInt(card.ID, 10)]
	return card.Unread || card.Pending || waiting || slices.Contains(eventIDs, card.ID)
}

func renderProject(project Project, notes map[string]Note, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s%s\n", project.Key, project.Place, activityAge(project.Activity, now))

	if note, ok := notes["project."+project.Key]; ok {
		text := note.Text
		if len(project.Sessions) == 0 {
			text = Clip(text, 120)
		}

		fmt.Fprintf(&b, "  project%s: %s\n", noteAge(note, now), text)
	}

	for _, card := range project.Sessions {
		renderCard(&b, card, notes, now)
	}

	if older := len(project.SessionIDs) - len(project.Sessions); older > 0 {
		fmt.Fprintf(&b, "  +%d older sessions\n", older)
	}

	return b.String()
}

func renderCard(b *strings.Builder, card Card, notes map[string]Note, now time.Time) {
	unread := ""
	if card.Unread {
		unread = "  [unread]"
	}

	signals := ""

	if card.ContextTokens > 0 {
		ctx := strconv.Itoa(card.ContextTokens)
		if card.ContextTokens >= 1000 {
			ctx = fmt.Sprintf("%gk", float64(card.ContextTokens)/1000)
		}

		signals += "  ctx " + ctx
	}

	if card.CostUSD > 0 {
		signals += fmt.Sprintf("  $%.2f", card.CostUSD)
	}

	fmt.Fprintf(b, "  %d  %s%s%s%s\n", card.ID, card.Status, activityAge(card.Activity, now), signals, unread)
	id := strconv.FormatInt(card.ID, 10)

	note, ok := notes["session."+id]
	if ok {
		fmt.Fprintf(b, "    session%s: %s\n", noteAge(note, now), note.Text)
	}

	if !ok {
		fmt.Fprintf(b, "    asked: %q\n", Clip(card.Asked, 120))
	}

	if !ok || card.LastFinalAt.Unix() > note.WrittenAt {
		fmt.Fprintf(b, "    last: %q\n", Clip(card.LastLine, 200))
	}

	if wait, ok := notes["wait."+id]; ok {
		fmt.Fprintf(b, "    ⏳ wait%s: %s\n", noteAge(wait, now), wait.Text)
	}
}

func noteMap(notes []Note) map[string]Note {
	indexed := make(map[string]Note, len(notes))
	for _, note := range notes {
		indexed[note.Key] = note
	}

	return indexed
}

func noteAge(note Note, now time.Time) string {
	if note.WrittenAt == 0 {
		return ""
	}

	return " (" + age(time.Unix(note.WrittenAt, 0), now) + ")"
}

func activityAge(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}

	return "  " + age(at, now)
}

func age(at, now time.Time) string {
	delta := now.Sub(at)
	if delta < time.Hour {
		return "now"
	}

	if delta < 24*time.Hour {
		return fmt.Sprintf("%dh", int(delta/time.Hour))
	}

	return fmt.Sprintf("%dd", int(delta/(24*time.Hour)))
}
