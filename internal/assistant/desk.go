package assistant

import (
	"fmt"
	"maps"
	"slices"
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
	Place    string
	Status   string
	Activity time.Time
	Asked    string
	LastLine string
	Unread   bool
}

// Event preserves the full input separately from its shorter tape representation.
type Event struct {
	LineID int64
	At     time.Time
	Kind   string
	Text   string
}

// Render produces the sole user message supplied on each assistant step.
func Render(d Desk, board []Card, tape []Line, event Event, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n\n## notebook (%d/30 entries, %d/2000 chars)\n",
		now.UTC().Format("2006-01-02 15:04 MST"), len(d.Diary), diaryChars(d.Diary))

	for _, key := range slices.Sorted(maps.Keys(d.Diary)) {
		fmt.Fprintf(&b, "%s: %s\n", key, d.Diary[key])
	}

	fmt.Fprintf(&b, "\n## stickers (%d/8, %d/1000 chars)\n", len(d.Stickers),
		utf8.RuneCountInString(strings.Join(d.Stickers, "")))

	for _, sticker := range d.Stickers {
		fmt.Fprintf(&b, "- %s\n", sticker)
	}

	fmt.Fprintf(&b, "\n## focus\n%s\n\n## board\n", d.Focus)

	for _, card := range board {
		unread := ""
		if card.Unread {
			unread = "  [unread]"
		}

		fmt.Fprintf(&b, "%d  %s  %s  %s  %s  asked: %q  last: %q%s\n", card.ID, card.Project, card.Place, card.Status,
			deskTime(card.Activity, now), Clip(card.Asked, 120), Clip(card.LastLine, 200), unread)
	}

	renderTape(&b, d.TapeLastID, tape, event, now)
	fmt.Fprintf(&b, "\n## event (line %d)\n%s\n\nAnswer with exactly one JSON object and nothing else.\n",
		event.LineID, ClipMiddle(event.Text, eventTextLimit))

	return b.String()
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
