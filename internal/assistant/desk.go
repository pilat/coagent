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
	deskTextLimit = 6000
	// A work answer is the thing to retell, so the event keeps its head and tail.
	eventTextLimit = 20000
)

// Card is the host's read-only projection of a work session.
type Card struct {
	ID      int64
	Project string
	// Place says what the project is: "repo <path>", "scratch" or "dir <path>".
	Place         string
	Status        string
	Activity      time.Time
	Asked         string
	LastLine      string
	Unread        bool
	ProjectKey    string
	ContextTokens int
	CostUSD       float64
	LastFinalAt   time.Time
	Pending       bool
}

// Event preserves the full input separately from its shorter tape representation.
type Event struct {
	LineID   int64
	At       time.Time
	Kind     string
	Text     string
	Sessions []int64
}

// Render produces the sole user message supplied on each assistant step.
func Render(d Desk, projects []Project, tape []Line, event Event, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n\n", now.UTC().Format("2006-01-02 15:04 MST"))
	renderNoteScopes(&b, d.Notes, now)
	fmt.Fprintf(&b, "\n## focus\n%s\n\n", d.Focus)
	shown := TrimCatalog(projects, d, event, now)

	b.WriteString(catalogHeader)

	notes := noteMap(d.Notes)
	for _, project := range shown.Projects {
		b.WriteString(renderProject(project, notes, now))
	}

	if shown.MoreProjects > 0 {
		fmt.Fprintf(&b, "+%d more projects\n", shown.MoreProjects)
	}

	renderOrphaned(&b, d.Notes, shown, now)
	renderTape(&b, d.TapeLastID, tape, event, now)
	fmt.Fprintf(&b, "\n## event (line %d)\n%s\n\nAnswer with exactly one JSON object and nothing else.\n",
		event.LineID, ClipMiddle(event.Text, eventTextLimit))

	return b.String()
}

// Edited stamps only the note keys written in this answer.
func (d Desk) Edited(a Answer, now time.Time) Desk {
	next := d

	indexed := noteMap(d.Notes)
	for key, text := range a.Notes {
		if text == nil {
			delete(indexed, key)
		} else {
			indexed[key] = Note{Key: key, Text: *text, WrittenAt: now.Unix()}
		}
	}

	next.Notes = nil
	for _, note := range indexed {
		next.Notes = append(next.Notes, note)
	}

	slices.SortFunc(next.Notes, func(a, b Note) int { return strings.Compare(a.Key, b.Key) })

	if a.Focus != nil {
		next.Focus = *a.Focus
	}

	return next
}

func renderNoteScopes(b *strings.Builder, notes []Note, now time.Time) {
	fmt.Fprintf(b, "## you (%d/1500)\n", scopeChars(notes, "user.")+scopeChars(notes, "rule."))
	ordered := slices.Clone(notes)
	slices.SortFunc(ordered, func(a, b Note) int { return strings.Compare(a.Key, b.Key) })

	for _, prefix := range []string{"user.", "rule.", "open."} {
		if prefix == "open." {
			fmt.Fprintf(b, "\n## open (%d/800)\n", scopeChars(notes, prefix))
		}

		for _, note := range ordered {
			if strings.HasPrefix(note.Key, prefix) {
				fmt.Fprintf(b, "%s%s: %s\n", note.Key, noteAge(note, now), note.Text)
			}
		}
	}
}

func renderOrphaned(b *strings.Builder, notes []Note, shown ShownCatalog, now time.Time) {
	var orphaned []Note

	for _, note := range notes {
		scope, target, _ := strings.Cut(note.Key, ".")
		missing := false

		switch scope {
		case noteScopeProject:
			missing = !shown.ProjectKeys[target]
		case noteScopeSession, noteScopeWait:
			id, err := strconv.ParseInt(target, 10, 64)
			missing = err != nil || !shown.SessionIDs[id]
		}

		if missing {
			orphaned = append(orphaned, note)
		}
	}

	if len(orphaned) == 0 {
		return
	}

	slices.SortFunc(orphaned, func(a, b Note) int { return strings.Compare(a.Key, b.Key) })
	b.WriteString("\n## orphaned\n")

	for _, note := range orphaned {
		fmt.Fprintf(b, "%s%s: %s\n", note.Key, noteAge(note, now), note.Text)
	}
}

func scopeChars(notes []Note, prefix string) int {
	total := 0

	for _, note := range notes {
		if strings.HasPrefix(note.Key, prefix) {
			total += utf8.RuneCountInString(note.Text)
		}
	}

	return total
}

// Clip limits Unicode characters, including the marker when text is shortened.
func Clip(text string, limit int) string {
	chars := []rune(text)
	if len(chars) <= limit {
		return text
	}

	const marker = "[…]"

	return string(chars[:max(0, limit-3)]) + marker
}

// ClipMiddle keeps the head and the tail so the end of a long text stays visible.
func ClipMiddle(text string, limit int) string {
	chars := []rune(text)
	if len(chars) <= limit {
		return text
	}

	const marker = "\n[…]\n"

	tail := max(0, limit/4)
	head := max(0, limit-tail-utf8.RuneCountInString(marker))

	return string(chars[:head]) + marker + string(chars[len(chars)-tail:])
}

func renderTape(b *strings.Builder, lastID int64, tape []Line, event Event, now time.Time) {
	lines := slices.Clone(tape)
	if event.LineID > 0 && !slices.ContainsFunc(lines, func(line Line) bool { return line.ID == event.LineID }) {
		lines = append(lines, Line{ID: event.LineID, At: event.At, Kind: event.Kind, Text: Clip(event.Text, 300)})
	}

	slices.SortFunc(lines, func(a, b Line) int {
		if a.ID < b.ID {
			return -1
		}

		if a.ID > b.ID {
			return 1
		}

		return 0
	})

	if len(lines) == 0 {
		b.WriteString("\n## tape (empty)\n")
		return
	}

	last := lines[len(lines)-1].ID
	fmt.Fprintf(b, "\n## tape (visible lines %d–%d of %d)\n", lines[0].ID, last, max(lastID, last))

	for _, line := range lines {
		fmt.Fprintf(b, "%d %s %s\n", line.ID, deskTime(line.At, now), Clip(line.Text, deskTextLimit))
	}
}

func deskTime(at, now time.Time) string {
	if at.UTC().Format(time.DateOnly) == now.UTC().Format(time.DateOnly) {
		return at.UTC().Format("15:04")
	}

	return at.UTC().Format("Jan 02 15:04")
}
