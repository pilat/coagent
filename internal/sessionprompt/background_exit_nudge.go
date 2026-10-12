package sessionprompt

import (
	"fmt"
	"strings"
	"time"
)

const commandExcerptRunes = 80

// LiveProcess names one background process still running when a subagent stopped.
type LiveProcess struct {
	ID      string
	Command string
	Running time.Duration
}

// RenderBackgroundExitNudge tells a subagent that stopping now does not deliver its result.
func RenderBackgroundExitNudge(processes []LiveProcess) string {
	var sb strings.Builder

	sb.WriteString("You ended your response while background processes you started are still running:\n")

	for _, process := range processes {
		fmt.Fprintf(&sb, "- %s (running %s)", process.ID, process.Running.Round(time.Second))

		if excerpt := commandExcerpt(process.Command); excerpt != "" {
			sb.WriteString(": " + excerpt)
		}

		sb.WriteString("\n")
	}

	sb.WriteString(
		"\nYour parent receives only your final answer, and it replaces everything you said before:" +
			" write it as the complete result with every finding and caveat, not only what changed since." +
			"\nIf you no longer need a process's result, cancel it with cancel_process, then give that final answer." +
			"\nIf you need a result, end your turn now: you are resumed when the process completes," +
			" and only then does your parent receive your result.",
	)

	return sb.String()
}

func commandExcerpt(command string) string {
	runes := []rune(strings.Join(strings.Fields(command), " "))
	if len(runes) <= commandExcerptRunes {
		return string(runes)
	}

	return string(runes[:commandExcerptRunes]) + "…"
}
