package sessionprompt

import (
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/todo"
)

// RenderCompletionNudge asks for a second look: a confirmation keeps the previous answer,
// a continuation must end in a complete replacement answer.
func RenderCompletionNudge(items []*todo.Item) string {
	var open []string

	sorted := append([]*todo.Item(nil), items...)
	todo.SortCanonical(sorted)

	for _, item := range sorted {
		if item.Status != todo.StatusPending && item.Status != todo.StatusInProgress {
			continue
		}

		open = append(open, fmt.Sprintf("- [%s] %s", item.Status, item.Content))
	}

	var sb strings.Builder

	sb.WriteString("You ended your previous response without calling a tool. ")

	if len(open) > 0 {
		sb.WriteString("Before stopping, re-check this open work:\n")
		sb.WriteString(strings.Join(open, "\n"))
		sb.WriteString(
			"\n\nIf action remains, continue with tools (reconcile completed, cancelled, or no-longer-applicable items first). ",
		)
	} else {
		sb.WriteString(
			"Re-check the original request and the work already done. If action remains, continue with tools. ",
		)
	}

	sb.WriteString(
		"Otherwise reply with one sentence confirming you are done; your previous answer is delivered as is and this confirmation is never shown.",
	)
	sb.WriteString(
		" If you continue with tools, your next text-only response ends the task without another check" +
			" and is delivered to the user in place of your previous answer: write it to the user as the complete answer" +
			" to the original request, keeping every finding and caveat from your previous answer, not as a report of this re-check.",
	)

	return sb.String()
}
