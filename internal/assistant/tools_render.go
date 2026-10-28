package assistant

import (
	"fmt"
	"strings"
	"time"
)

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

func renderSessions(cards []Card, offset, total int, now time.Time) string {
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
		b.WriteString(cardLines(card, now))
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
