package assistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/sessionstore"
)

const toldLookback = 200

// alreadyTold notes, inside an answer event, what the assistant already said about that
// session after reading it: a read retold before the answer arrives is otherwise retold twice.
func (l *loop) alreadyTold(ctx context.Context, input *sessionstore.InboxInput) (string, error) {
	if kind, _ := input.Attributes["assistant_event"].(string); kind != "answered" {
		return "", nil
	}

	id, ok := attributeID(input.Attributes["session_id"])
	if !ok {
		return "", nil
	}

	lines, err := l.deps.Memory.RecentTape(ctx, l.sessionID, toldLookback)
	if err != nil {
		return "", fmt.Errorf("load told lines: %w", err)
	}

	marker := fmt.Sprintf("(%d)", id)
	readAt := -1

	for i, line := range lines {
		switch {
		case line.Kind == moveKind && strings.HasPrefix(line.Text, "→ ") && strings.Contains(line.Text, marker):
			readAt = -1
		case line.Kind == observeKind && strings.HasPrefix(line.Text, "read ") && strings.Contains(line.Text, marker):
			readAt = i
		}
	}

	if readAt < 0 {
		return "", nil
	}

	for _, line := range lines[readAt+1:] {
		if line.Kind == sayKind {
			return fmt.Sprintf("Host note: after reading this session at %s you already told the user: %s "+
				"Add only what is new, or say nothing.", line.At.UTC().Format("15:04"), Clip(line.Text, 400)), nil
		}
	}

	return "", nil
}

func attributeID(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), v > 0
	default:
		return 0, false
	}
}
