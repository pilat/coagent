package session

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/progress"
)

const agentsMDMessagePrefix = "User preferences from AGENTS.md files (lower priority than system instructions):\n\n"

const noTaskPrompt = "You were just started but the user hasn't provided a task yet. " +
	"Greet them briefly and wait for their instructions."

// sessionStatus holds session statistics for /status command. Two honest numbers:
// current window occupancy (this turn) and lifetime session total (all-in, from DB).
type sessionStatus struct {
	Model         string
	LifetimeIn    int     // lifetime prompt tokens, whole tree, incl compaction (billed throughput)
	LifetimeOut   int     // lifetime completion tokens
	LifetimeCost  float64 // lifetime cost USD, all-in
	ContextUsed   int     // projected next-request input (same number the trigger uses)
	ContextMax    int     // context window (same source as the compaction trigger)
	ContextIsEst  bool    // no provider measurement backs ContextUsed
	Iteration     int
	SubagentCount int
}

func (s *svc) ContextProjection(ctx context.Context) progress.Context {
	status := s.buildSessionStatus(ctx)

	return progress.Context{
		Used: status.ContextUsed, Max: status.ContextMax, Approximate: status.ContextIsEst,
		Available: status.ContextMax > 0 && status.ContextUsed > 0,
	}
}

// lastUserMessage returns the content of the last user message in the history.
func lastUserMessage(messages []llmwire.Message) string {
	for _, v := range slices.Backward(messages) {
		if v.Role == llmwire.RoleUser {
			return v.Content
		}
	}

	return ""
}

// lastAssistantTextOnly returns the text of the last assistant message if it has
// no tool calls. Returns "" otherwise.
func lastAssistantTextOnly(messages []llmwire.Message) string {
	for _, v := range slices.Backward(messages) {
		switch v.Role {
		case llmwire.RoleAssistant:
			if len(v.ToolCalls) == 0 && v.Content != "" {
				return v.Content
			}

			return ""
		case llmwire.RoleUser:
			return ""
		}
	}

	return ""
}

// buildSessionStatus reports the compaction trigger's own projection and the
// lifetime tree-sum. A backward usage scan would read 0% right after a compaction.
func (s *svc) buildSessionStatus(ctx context.Context) sessionStatus {
	s.modelMu.RLock()
	model := s.model
	s.modelMu.RUnlock()

	contextUsed, estimated := s.projectContextSize()

	var lifetimeIn, lifetimeOut int
	var lifetimeCost float64
	subagentCount := 0

	if s.store != nil {
		if in, out, cost, err := s.store.GetSessionTreeUsage(ctx, s.rootID); err == nil {
			lifetimeIn, lifetimeOut, lifetimeCost = in, out, cost
		}

		if childCount, _, err := s.store.GetChildSessionStats(ctx, s.rootID); err == nil {
			subagentCount = childCount
		}
	}

	return sessionStatus{
		Model:         model,
		LifetimeIn:    lifetimeIn,
		LifetimeOut:   lifetimeOut,
		LifetimeCost:  lifetimeCost,
		ContextUsed:   contextUsed,
		ContextMax:    s.contextWindow(),
		ContextIsEst:  estimated,
		Iteration:     s.iterationOffset,
		SubagentCount: subagentCount,
	}
}

const statusBarCells = 10

// renderStatus builds the controller-agnostic Markdown /status view: a backtick
// occupancy bar (HTML-escape-safe) headlined by lifetime cost. Pure and testable.
func renderStatus(st sessionStatus) string {
	pct := 0
	if st.ContextMax > 0 && st.ContextUsed > 0 {
		pct = min(100, st.ContextUsed*100/st.ContextMax)
	}

	filled := min(statusBarCells, int(math.Round(float64(pct)/10.0)))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", statusBarCells-filled)

	band := "🟢"
	tail := ""

	switch {
	case pct >= int(compactionFraction*100):
		band = "🔴"
		tail = " · compacting soon"
	case pct >= 70:
		band = "🟡"
	}

	var sb strings.Builder

	sb.WriteString("📊 **Session Status**\n\n")
	fmt.Fprintf(&sb, "- **Model**: %s\n", st.Model)
	fmt.Fprintf(&sb, "- **Iterations**: %d\n", st.Iteration)

	if st.SubagentCount > 0 {
		fmt.Fprintf(&sb, "- **Subagents**: %d\n", st.SubagentCount)
	}

	// A tilde marks a pure estimate, never mistakable for a reported number.
	approx := ""
	if st.ContextIsEst {
		approx = "~"
	}

	fmt.Fprintf(&sb, "\n%s Context `%s` %s%d%% (%s%s / %s)%s\n",
		band, bar, approx, pct, approx, formatTokens(st.ContextUsed), formatTokens(st.ContextMax), tail)
	fmt.Fprintf(&sb, "\nLifetime (all-in): **$%.2f** · %s in · %s out\n",
		st.LifetimeCost, formatTokens(st.LifetimeIn), formatTokens(st.LifetimeOut))

	return sb.String()
}

// formatTokens renders a token count with a k/M suffix; counts under 1000 stay plain.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

// openingTurn assembles the turn that opens a conversation — AGENTS.md header
// (when present) plus the stamped task. Pure: no IO, no store mutation.
func (s *svc) openingTurn(prompt string) []llmwire.Message {
	msgs := make([]llmwire.Message, 0, 2)

	if s.agentsMD != "" {
		msgs = append(msgs, llmwire.Message{
			Role:    llmwire.RoleUser,
			Content: agentsMDMessagePrefix + s.agentsMD,
		})
	}

	if prompt == "" {
		prompt = noTaskPrompt
	}

	return append(msgs, llmwire.Message{Role: llmwire.RoleUser, Content: s.stamper.stamp(prompt)})
}
