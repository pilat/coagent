package sessionprompt

import (
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/todo"
)

// RenderCompletionNudge asks for a second look while permitting an explained stop.
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

	sb.WriteString("Otherwise answer once more with a concise explanation of why you are stopping.")

	return sb.String()
}
