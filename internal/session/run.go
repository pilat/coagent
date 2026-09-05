package session

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const agentsMDMessagePrefix = "User preferences from AGENTS.md files (lower priority than system instructions):\n\n"

const noTaskPrompt = "You were just started but the user hasn't provided a task yet. " +
	"Greet them briefly and wait for their instructions."

// RunResult is returned by RunDaemon when the session's ReAct loop exits.
type RunResult struct {
	FinalResponse string
	Suspended     bool
	ErrorNotice   string

	// CompactionDeferAnnounced hands the deferral notice's dedup state back to
	// the daemon, which owns the session's identity across wakes.
	CompactionDeferAnnounced bool
}

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

// Run executes the main agent loop.
func (s *svc) Run(ctx context.Context, prompt string) (string, error) {
	result, err := s.run(ctx, prompt)
	if err != nil {
		return "", err
	}

	return result.FinalResponse, nil
}

// run preserves the loop's discriminated result for daemon callers. Run keeps
// the historical text-only API for direct callers, while RunDaemon must carry
// suspension across the session boundary without reconstructing it from
// producer ledgers that may have been created during this run.
//
//nolint:funlen // Run keeps setup, loop, and durable terminalization in causal order.
func (s *svc) run(ctx context.Context, prompt string) (*loopResult, error) {
	ctx = logger.With(ctx, zap.Int64("session_id", s.rootID), zap.Int64("agent_id", s.id))
	log := logger.Ctx(ctx).Named("session.run")

	// Last point before the first request where the registry can still grow: the
	// daemon registers its tools between construction and this call.
	s.refreshRegistrySections()

	activationIndex, err := tool.ActivationIndex(s.registry)
	if err != nil {
		return nil, fmt.Errorf("index activation commands: %w", err)
	}

	s.activationIndex = activationIndex
	if boundary, ok := s.boundary.(activationStateBoundary); ok {
		grant, loadErr := boundary.PendingActivation(ctx)
		if loadErr != nil {
			return nil, fmt.Errorf("load pending activation: %w", loadErr)
		}

		if grant != nil && grant.ToolCallID != "" {
			pending := unresolvedToolCalls(s.ms.getMessages())
			if pending[grant.ToolCallID] != grant.ToolID {
				grant = nil
			}
		}

		s.currentActivation = grant
	}

	if err := s.prepareRunMessages(ctx, prompt); err != nil {
		return nil, err
	}

	s.suspended = false

	result, err := runLoop(
		ctx,
		s,
		s.loopOpts,
		func(iteration int, response *llmwire.Response, toolCalls []llmwire.ToolCall) error {
			totalIteration := s.iterationOffset + iteration

			s.stamper.touch()
			log.Info("iteration", zap.Int("iter", iteration))

			if response.Thoughts != "" {
				log.Debug("thoughts", zap.String("text", response.Thoughts))
			}

			if response.Text != "" {
				log.Info("response", zap.String("text", response.Text))
			}

			if len(toolCalls) > 0 {
				for _, tc := range toolCalls {
					args := logger.FormatArgs(tc.Arguments, 200)
					log.Info("tool_call", zap.String("name", tc.Name), zap.String("args", args))
				}
			}

			if saveErr := s.persistState(ctx, totalIteration, s.activationStatus()); saveErr != nil {
				return fmt.Errorf("persist checkpoint (iteration %d): %w", totalIteration, saveErr)
			}

			return nil
		},
	)

	totalIterations := s.iterationOffset + result.Iterations

	if err != nil {
		notice := result.ErrorNotice
		if notice == "" {
			notice = fmt.Sprintf(
				"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
				logger.Redact(err.Error()),
			)
			result.ErrorNotice = notice
		}

		if saveErr := s.persistErrorState(ctx, totalIterations, notice); saveErr != nil {
			return nil, errors.Join(err, fmt.Errorf("persist checkpoint error state: %w", saveErr))
		}

		return result, err
	}

	finalStatus := sessionstore.SessionStatusCompleted
	if result.Suspended {
		finalStatus = sessionstore.SessionStatusSuspended
	}

	// A command-only activation of a stopped root (e.g. /status) must not
	// reactivate it: persisting completed would soft-resume a root the user parked.
	if s.preserveStopped {
		finalStatus = sessionstore.SessionStatusStopped
	}

	if saveErr := s.persistState(ctx, totalIterations, finalStatus); saveErr != nil {
		return nil, fmt.Errorf("persist checkpoint %s state: %w", finalStatus, saveErr)
	}

	return result, nil
}

// activationStatus is the status a per-iteration checkpoint writes while the
// session runs: active, or stopped when the activation is command-only.
func (s *svc) activationStatus() sessionstore.SessionStatus {
	if s.preserveStopped {
		return sessionstore.SessionStatusStopped
	}

	return sessionstore.SessionStatusActive
}

func (s *svc) prepareRunMessages(ctx context.Context, prompt string) error {
	if len(s.ms.getMessages()) != 0 {
		if prompt == "" {
			return nil
		}

		stamped := s.stamper.stamp(prompt)
		if err := s.ms.addUserMessage(ctx, s.appendGitStateDelta(ctx, stamped)); err != nil {
			return fmt.Errorf("inject user message: %w", err)
		}

		return nil
	}

	if s.boundary != nil {
		if err := s.initFreshBoundarySession(ctx); err != nil {
			return fmt.Errorf("init fresh boundary session: %w", err)
		}

		return nil
	}

	if err := s.initFreshSession(ctx, prompt); err != nil {
		return fmt.Errorf("init fresh session: %w", err)
	}

	return nil
}

// RunDaemon runs the session with durable boundary input and notifications.
func (s *svc) RunDaemon(
	ctx context.Context,
	notify func(sessionevent.Notification),
) (RunResult, error) {
	if notify != nil {
		s.loopOpts = loopOptions{
			Notify: func(_ context.Context, message string) error {
				notify(sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
				return nil
			},
			Heartbeat: func(_ context.Context) {
				notify(sessionevent.Notification{Type: sessionevent.NotifyHeartbeat})
			},
		}
	}

	result, err := s.run(ctx, "")
	if err != nil {
		out := RunResult{CompactionDeferAnnounced: s.compactionDeferAnnounced}
		if result != nil {
			out.ErrorNotice = result.ErrorNotice
		}

		return out, err
	}

	return RunResult{
		FinalResponse:            result.FinalResponse,
		Suspended:                result.Suspended,
		CompactionDeferAnnounced: s.compactionDeferAnnounced,
	}, nil
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

// initFreshSession prepopulates the message store for a brand-new session.
func (s *svc) initFreshSession(ctx context.Context, prompt string) error {
	for _, msg := range s.openingTurn(prompt) {
		if err := s.ms.addUserMessage(ctx, msg.Content); err != nil {
			return err
		}
	}

	return nil
}

func (s *svc) initFreshBoundarySession(ctx context.Context) error {
	if s.agentsMD == "" {
		return nil
	}

	return s.ms.addUserMessage(ctx, agentsMDMessagePrefix+s.agentsMD)
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
