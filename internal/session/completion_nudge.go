package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/todo"
)

// completionNudgePending reports whether the transcript tail is host-authored
// second-look material. A pending durable candidate is the typed proof: every
// external model-visible input clears the candidate in its own commit, so any
// user row after it is a host nudge, never a live user question. A store
// failure answers false, so at worst a read-only command costs one extra
// model call rather than skipping an owed one.
func (r *loopRunner) completionNudgePending(ctx context.Context) bool {
	state, err := r.completionState(ctx)
	if err != nil || state == nil || state.CandidateID == nil {
		return false
	}

	messages := r.agent.ms.getMessages()

	return len(messages) > 0 && messages[len(messages)-1].Role == llmwire.RoleUser
}

// renderCompletionNudge builds the single host-authored second-look prompt.
// Open items name only pending and in_progress entries in canonical order;
// with none open the todo clause is omitted entirely. The prompt permits a
// deliberate second stop with explanation and never requires exact text.
func renderCompletionNudge(items []*todo.Item) string {
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
