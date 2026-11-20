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
		"\nIf you are waiting for a result: reply with one word and stop. Nothing is sent to your parent yet;" +
			" you are resumed when the process completes, and your answer after that is the one your parent receives." +
			"\nIf you no longer need a result: cancel the process with cancel_process, then give your final answer." +
			"\nThat final answer is all your parent receives, and it replaces everything you said before:" +
			" write it as the complete result with every finding and caveat, not only what changed since.",
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
