package assistant

import (
	"cmp"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/catalog"
)

const (
	dialogTextLimit = 2000
	deskFocusItems  = 10
	toolTime        = "Jan 02 15:04"
	// RecentSessions is how many assistant-touched sessions the desk shows.
	RecentSessions = 10
)

// Card is the host's read-only projection of a work session.
type Card struct {
	ID, projectID               int64
	Project, Status             string
	CreatedAt, Activity         time.Time
	ContextTokens               int // Carried into every further step; zero when unknown.
	Pending, Scheduled, touched bool
	Title, Model, Effort        string
	Budget                      *budget.Record
	TreeCostUSD                 *float64
}

// Project is one place work can be opened; Key is what open and sessions accept.
type Project struct {
	Name, Key, Place string // Place is "repo <path>", "scratch", or "dir <path>".
	Activity         time.Time
}

// View is everything one desk render shows.
type View struct {
	Desk   Desk
	Recent []Card
	Total  int
	Models []Model
}

// Render produces the turn's first user message; every truncated list names the tool that shows more.
func Render(v View, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "now: %s\n\n", now.UTC().Format("2006-01-02 15:04 MST"))
	renderMemory(&b, v.Desk.Memory)
	renderFocus(&b, v.Desk.Focus, deskFocusItems, now)
	renderRecent(&b, v.Recent, v.Total, now, v.Models)
	renderConversation(&b, v.Desk.Conversation, v.Desk.Spoken, now)

	return b.String()
}

// Clip limits Unicode characters, including the marker when text is shortened.
func Clip(text string, limit int) string {
	chars := []rune(text)
	if len(chars) > limit {
		return string(chars[:max(0, limit-3)]) + "[…]"
	}

	return text
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

func renderRecent(b *strings.Builder, cards []Card, total int, now time.Time, models []Model) {
	fmt.Fprintf(b, "\n## recent sessions (%d of %d — sessions lists all work, search finds older)\n",
		len(cards), total)

	for _, card := range cards {
		b.WriteString(cardLines(card, now, models))
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

func cardLines(card Card, now time.Time, models []Model) string {
	line := fmt.Sprintf("%d %s · %s · created %s · touched %s", card.ID, card.Project, card.Status,
		card.CreatedAt.UTC().Format("Jan 02"), age(card.Activity, now))
	if card.ContextTokens > 0 {
		line += fmt.Sprintf(" · ctx %dk", max(1, (card.ContextTokens+500)/1000))
	}

	if model := cardModel(card, models); model != "" {
		line += " · " + model
	}

	if remaining := cardBudget(card, now); remaining != "" {
		line += " · " + remaining
	}

	if card.Scheduled {
		line += " · ⏰"
	}

	if card.Pending {
		line += " · ⏳ awaiting answer"
	}

	if card.Title != "" {
		line += "\n    about: " + card.Title
	}

	return line + "\n"
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

func cardModel(card Card, models []Model) string {
	for _, model := range models {
		if model.ID != card.Model {
			continue
		}

		name := cmp.Or(model.DisplayName, model.ID)
		if len(model.EffortLevels) > 0 {
			name += " · " + catalog.ClampEffort(catalog.StoredEffort(card.Effort), model.EffortAllowlist)
		}

		return name
	}

	return card.Model
}

func cardBudget(card Card, now time.Time) string {
	if card.Budget == nil {
		return ""
	}

	record := card.Budget
	if record.State == budget.Fired {
		state := "fired"
		if record.ParkPhase == "parked" {
			state = "parked"
		}

		return fmt.Sprintf("budget %s (%s)", state, record.FiredReason)
	}

	if record.State != budget.Armed {
		return ""
	}

	treeCost := 0.0
	if card.TreeCostUSD != nil {
		treeCost = *card.TreeCostUSD
	}

	cost, duration := record.Remaining(treeCost, now)

	var parts []string
	if cost != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *cost))
	}

	if duration != nil {
		minutes := int(*duration / time.Minute)

		text := fmt.Sprintf("%dm", minutes)
		if minutes >= 60 {
			text = fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
		}

		parts = append(parts, text)
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " · ") + " left"
}

func renderHits(query string, hits []Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("nothing matches %q. Try other stems with * in Russian and English; if nothing turns "+
			"up, write down that it was not found (remember with until).", query)
	}

	var b strings.Builder

	for _, hit := range hits {
		switch {
		case strings.HasPrefix(hit.Ref, "msg "):
			fmt.Fprintf(&b, "%s · %d %s · %s · %s: %s\n", hit.Ref, hit.Session, hit.Project, hit.Kind,
				hit.At.UTC().Format(toolTime), hit.Snippet)
		default:
			fmt.Fprintf(&b, "%s · %s · %s: %s\n", hit.Ref, hit.Kind, hit.At.UTC().Format(toolTime), hit.Snippet)
		}
	}

	return b.String()
}

func renderSessions(cards []Card, offset, total int, now time.Time, models []Model) string {
	if len(cards) == 0 {
		return fmt.Sprintf("no sessions at offset %d of %d", offset, total)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "sessions %d–%d of %d", offset+1, offset+len(cards), total)

	if next := offset + len(cards); next < total {
		fmt.Fprintf(&b, " (offset %d for more)", next)
	}

	b.WriteString(":\n")

	for _, card := range cards {
		b.WriteString(cardLines(card, now, models))
	}

	return b.String()
}

func renderProjects(projects []Project, now time.Time) string {
	if len(projects) == 0 {
		return "no projects yet; open with a new name creates a scratch folder"
	}

	var b strings.Builder

	for _, project := range projects {
		fmt.Fprintf(&b, "%s  %s", project.Key, project.Place)

		if !project.Activity.IsZero() {
			fmt.Fprintf(&b, "  active %s", age(project.Activity, now))
		}

		b.WriteString("\n")
	}

	return b.String()
}

func renderFocusList(items []Focus, now time.Time) string {
	if len(items) == 0 {
		return "focus is empty"
	}

	var b strings.Builder

	for _, item := range items {
		fmt.Fprintf(&b, "%s\n", focusLine(fmt.Sprintf("f%d", item.ID), item, now))
	}

	return b.String()
}

func renderModels(models []Model) string {
	lines := make([]string, 0, len(models))
	for i, model := range models {
		line := model.ID + " · " + cmp.Or(model.DisplayName, model.ID)
		if i == 0 {
			line += " · default"
		}

		if len(model.EffortLevels) == 0 {
			line += " · no effort choice"
		} else {
			efforts := strings.Join(model.EffortLevels, ", ")
			line += " · efforts: " + efforts + " · default effort: " + model.DefaultEffort
		}

		if model.Reasoning {
			line += " · reasoning"
		}

		if model.Priced {
			line += fmt.Sprintf(" · $%g/$%g", model.InputPrice, model.OutputPrice)
		} else {
			line += " · unpriced"
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}
