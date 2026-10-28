package assistant

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	dialogTextLimit  = 2000
	deskFocusItems   = 10
	memoryNoteChars  = 300
	memoryTotalChars = 4000
	toolTime         = "Jan 02 15:04"
	// RecentSessions is how many assistant-touched sessions the desk shows.
	RecentSessions = 10
)

// Card is the host's read-only projection of a work session.
type Card struct {
	ID        int64
	Project   string
	Status    string
	CreatedAt time.Time
	Activity  time.Time
	// ContextTokens is what every further step of the session carries; zero when unknown.
	ContextTokens int
	Pending       bool
	About         string
	projectID     int64
	touched       bool
}

// Project is one place work can be opened; Key is what open and sessions accept.
type Project struct {
	Name string
	Key  string
	// Place says what the project is: "repo <path>", "scratch" or "dir <path>".
	Place    string
	Activity time.Time
}

// View is everything one desk render shows.
type View struct {
	Desk   Desk
	Recent []Card
	Total  int
}

// Render produces the turn's first user message; every truncated list names the tool that shows more.
func Render(v View, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n\n", now.UTC().Format("2006-01-02 15:04 MST"))
	renderMemory(&b, v.Desk.Memory)
	renderFocus(&b, v.Desk.Focus, deskFocusItems, now)
	renderRecent(&b, v.Recent, v.Total, now)
	renderConversation(&b, v.Desk.Conversation, v.Desk.Spoken, now)

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

func renderMemory(b *strings.Builder, notes []Memory) {
	total := 0
	for _, note := range notes {
		total += utf8.RuneCountInString(note.Text)
	}

	fmt.Fprintf(b, "## memory (%d/%d)\n", total, memoryTotalChars)

	for _, note := range notes {
		fmt.Fprintf(b, "%s\n", memoryLine(fmt.Sprintf("m%d", note.ID), note))
	}
}

func renderFocus(b *strings.Builder, items []Focus, limit int, now time.Time) {
	switch {
	case len(items) == 0:
		b.WriteString("\n## focus (empty)\n")
	case len(items) > limit:
		fmt.Fprintf(b, "\n## focus (%d of %d — focus_list shows all)\n", limit, len(items))
	default:
		fmt.Fprintf(b, "\n## focus (%d)\n", len(items))
	}

	for _, item := range items[:min(limit, len(items))] {
		fmt.Fprintf(b, "%s\n", focusLine(fmt.Sprintf("f%d", item.ID), item, now))
	}
}

func renderRecent(b *strings.Builder, cards []Card, total int, now time.Time) {
	fmt.Fprintf(b, "\n## recent sessions (%d of %d — sessions lists all work, search finds older)\n",
		len(cards), total)

	for _, card := range cards {
		b.WriteString(cardLines(card, now))
	}
}

func renderConversation(b *strings.Builder, lines []Line, total int, now time.Time) {
	switch {
	case len(lines) == 0:
		b.WriteString("\n## conversation (empty)\n")
	case total > len(lines):
		fmt.Fprintf(b, "\n## conversation (last %d of %d — search finds older)\n", len(lines), total)
	default:
		fmt.Fprintf(b, "\n## conversation (last %d)\n", len(lines))
	}

	for _, line := range lines {
		role := "user"
		if line.Kind == sayKind {
			role = "you"
		}

		fmt.Fprintf(b, "%s %s\n", deskTime(line.At, now), Clip(role+": «"+line.Text+"»", dialogTextLimit))
	}
}

func memoryLine(ref string, note Memory) string {
	lifetime := "permanent"
	if note.Until != "" {
		lifetime = "until " + note.Until
	}

	return fmt.Sprintf("[%s] (%s) %s", ref, lifetime, note.Text)
}

func focusLine(ref string, item Focus, now time.Time) string {
	line := fmt.Sprintf("[%s] %s · %s", ref, age(item.UpdatedAt, now), item.Text)
	if len(item.Sessions) > 0 {
		line += " → " + strings.Join(item.Sessions, ", ")
	}

	return line
}

func cardLines(card Card, now time.Time) string {
	line := fmt.Sprintf("%d %s · %s · created %s · touched %s", card.ID, card.Project, card.Status,
		card.CreatedAt.UTC().Format("Jan 02"), age(card.Activity, now))
	if card.ContextTokens > 0 {
		line += fmt.Sprintf(" · ctx %dk", max(1, (card.ContextTokens+500)/1000))
	}

	if card.Pending {
		line += " · ⏳ awaiting answer"
	}

	if card.About == "" {
		return line + "\n"
	}

	return line + "\n    about: " + card.About + "\n"
}

func deskTime(at, now time.Time) string {
	if at.UTC().Format(time.DateOnly) == now.UTC().Format(time.DateOnly) {
		return at.UTC().Format("15:04")
	}

	return at.UTC().Format(toolTime)
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
