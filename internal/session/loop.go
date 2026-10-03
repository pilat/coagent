package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const workingAttribute = "working"

//nolint:gosec // Loop-detection instructions contain no credentials.
const (
	hardIterationCeiling        = 1000
	emptyResponseWarnThreshold  = 3
	emptyResponseBreakThreshold = 6
	compactionAttemptCap        = 3
	loopWarningTemplate         = "[LOOP WARNING: Low action diversity (%d%%). Your recent %d tool calls produced only %d unique outcomes.\n\nREQUIRED: Before your next tool call, explain in text WHY your current approach is not working and WHAT specifically you will change. Do not repeat the same strategy.]"
	loopBlockMessage            = "[BLOCKED: Tool execution blocked — you were warned about repetitive behavior but continued the same pattern. You MUST respond with text explaining your situation. No tool calls will be executed until you demonstrate a new approach.]"
	loopFailureWarningTemplate  = "[LOOP WARNING: The %s tool has returned the same error %d times in a row. Repeating the identical call will not help — fix the arguments or change your approach, or stop and explain the problem in text.]"
)

const agentsMDMessagePrefix = "User preferences from AGENTS.md files (lower priority than system instructions):\n\n"

const noTaskPrompt = "You were just started but the user hasn't provided a task yet. " +
	"Greet them briefly and wait for their instructions."

const statusBarCells = 10

type RunResult struct {
	Suspended            bool
	BudgetFired          bool
	Final                string
	ErrorNotice          string
	DeferNoticeAnnounced bool
}

type runState struct {
	result             RunResult
	iterations         int
	terminal           bool
	terminalState      bool
	handledControl     bool
	boundaryAgain      bool
	directReply        bool
	backgroundInserted bool
	compactionFailures int
	autoCompactionOff  bool
}

// Context reports the live projection without exposing progress-runtime policy.
type Context struct {
	Used, Max              int
	Approximate, Available bool
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

func (s *Session) Run(ctx context.Context) (RunResult, error) {
	ctx = logger.With(ctx)
	if err := s.prepareRun(ctx); err != nil {
		return RunResult{}, err
	}

	r := &runState{}

	defer s.emit(
		sessionevent.Notification{
			Type:       sessionevent.NotifyModelWorking,
			Attributes: map[string]any{workingAttribute: false},
		},
	)
	defer s.startHeartbeat(ctx)()

	if !s.HasPendingExternalCall() {
		s.compactionDeferAnnounced = false
	}

	return s.finishRun(ctx, r, s.runIterations(ctx, r))
}

func (s *Session) ContextProjection(ctx context.Context) Context {
	status := s.buildSessionStatus(ctx)

	return Context{
		Used: status.ContextUsed, Max: status.ContextMax, Approximate: status.ContextIsEst,
		Available: status.ContextMax > 0 && status.ContextUsed > 0,
	}
}

func (s *Session) prepareRun(ctx context.Context) error {
	index, err := tool.ActivationIndex(s.registry)
	if err != nil {
		return fmt.Errorf("prepare run: %w", err)
	}

	s.activationIndex = index

	return s.loadPendingActivation(ctx)
}

func (s *Session) runIterations(ctx context.Context, r *runState) error {
	for r.iterations < hardIterationCeiling {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}

		again, err := s.runIteration(ctx, r)
		if err != nil || !again {
			return err
		}
	}

	return nil
}

func (s *Session) runIteration(ctx context.Context, r *runState) (bool, error) {
	accepted, err := s.boundaryStep(ctx, r)
	if err != nil {
		return false, err
	}

	if r.boundaryAgain {
		return true, nil
	}

	if s.HasPendingExternalCall() {
		r.result.Suspended = true
		return false, nil
	}

	if len(s.pendingInLoopCalls()) > 0 {
		return s.runPendingTools(ctx, r)
	}

	if !accepted && (r.handledControl || !s.unansweredWork()) {
		return false, s.compactionStep(ctx, r)
	}

	admitted, err := s.admitModelStep(ctx, r)
	if err != nil || !admitted {
		return false, err
	}

	s.emit(
		sessionevent.Notification{
			Type:       sessionevent.NotifyModelWorking,
			Attributes: map[string]any{workingAttribute: true},
		},
	)

	if err := s.modelStep(ctx, r); err != nil {
		return false, err
	}

	if r.terminal || r.result.BudgetFired {
		return false, nil
	}

	return s.runPendingTools(ctx, r)
}

func (s *Session) runPendingTools(ctx context.Context, r *runState) (bool, error) {
	if calls := s.pendingInLoopCalls(); len(calls) > 0 {
		if err := s.toolStep(ctx, calls); err != nil {
			return false, err
		}
	}

	if s.suspended {
		r.result.Suspended = true
		return false, nil
	}

	return true, nil
}

func (s *Session) admitModelStep(ctx context.Context, r *runState) (bool, error) {
	s.applyModelSwitch()

	if err := s.compactionStep(ctx, r); err != nil {
		return false, err
	}

	if s.budgetFired {
		r.result.BudgetFired = true
		return false, nil
	}

	fired, err := s.observeBudget(ctx)
	if err != nil {
		return false, err
	}

	r.result.BudgetFired = fired

	return !fired, nil
}

func (s *Session) emit(n sessionevent.Notification) {
	if s.events != nil {
		s.events.Emit(n)
	}
}

func (s *Session) newCommit() sessionstore.Commit {
	return sessionstore.Commit{SessionID: s.id, RootID: s.rootID, At: time.Now().UTC()}
}

func (s *Session) commit(ctx context.Context, c sessionstore.Commit) (*sessionstore.CommitResult, error) {
	for _, output := range append(append([]sessionstore.Output{}, c.Outputs...), c.Unfired.Outputs...) {
		if output.ReleasesInput {
			s.emit(
				sessionevent.Notification{
					Type:       sessionevent.NotifyModelWorking,
					Attributes: map[string]any{workingAttribute: false},
				},
			)

			break
		}
	}

	result, err := s.store.Commit(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.budgetFired = s.budgetFired || result.BudgetFired
	if err := s.ms.reloadMessages(ctx); err != nil {
		return nil, err
	}

	if c.ObserveBudget && c.State.Iteration != nil {
		s.emit(
			sessionevent.Notification{
				Type:       sessionevent.NotifyIterationPersisted,
				Attributes: map[string]any{"iteration": *c.State.Iteration},
			},
		)
	}

	s.emit(sessionevent.Notification{Type: sessionevent.NotifyContextChanged})
	s.emitCommitted(s.liveOutputs(c, result), result.BudgetFired)

	return result, nil
}

func (s *Session) liveOutputs(c sessionstore.Commit, result *sessionstore.CommitResult) []*sessionstore.OutputCommit {
	outputs := append([]sessionstore.Output{}, c.Outputs...)
	if !result.BudgetFired {
		outputs = append(outputs, c.Unfired.Outputs...)
	}

	if s.outputEnabled || len(result.Outputs) > 0 {
		live := make([]*sessionstore.OutputCommit, 0, len(result.Outputs))
		for _, output := range result.Outputs {
			if !output.PersistOnly {
				live = append(live, output)
			}
		}

		return live
	}

	live := make([]*sessionstore.OutputCommit, 0, len(outputs))
	for _, output := range outputs {
		if !output.PersistOnly {
			live = append(live, &sessionstore.OutputCommit{LiveContent: output.Content})
		}
	}

	return live
}

func (s *Session) observeBudget(ctx context.Context) (bool, error) {
	result, err := s.store.Commit(ctx, sessionstore.Commit{SessionID: s.id, RootID: s.rootID, ObserveBudget: true})
	if err != nil {
		return false, fmt.Errorf("observe budget: %w", err)
	}

	return result.BudgetFired, nil
}

func (s *Session) loadPendingActivation(ctx context.Context) error {
	activation, err := s.store.PendingActivation(ctx, s.id)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load pending activation: %w", err)
	}

	if activation == nil {
		return nil
	}

	s.currentActivation = &tool.ActivationGrant{
		SessionID:  activation.SessionID,
		InputID:    activation.InputID,
		ToolID:     activation.ToolID,
		Command:    activation.Command,
		ToolCallID: activation.ToolCallID,
	}

	return nil
}

func (s *Session) expireActivation(ctx context.Context, suspended bool) error {
	grant := s.currentActivation
	if grant == nil || grant.ToolCallID != "" {
		return nil
	}

	if suspended {
		for _, call := range s.PendingExternalCalls() {
			if call.Name == grant.ToolID {
				return nil
			}
		}
	}

	c := s.newCommit()
	c.Activation = &sessionstore.ActivationChange{
		InputID: grant.InputID,
		State:   sessionstore.ActivationExpired,
		ToolID:  grant.ToolID,
		Command: grant.Command,
	}

	c.Outputs = []sessionstore.Output{
		{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       grant.Command + " was not changed",
			Key:           fmt.Sprintf("input:%d:activation:expired", grant.InputID),
			MessageRef:    -1,
			ReleasesInput: true,
		},
	}
	if _, err := s.commit(ctx, c); err != nil {
		return err
	}

	s.currentActivation = nil

	return nil
}

func (s *Session) emitCommitted(outputs []*sessionstore.OutputCommit, fired bool) {
	if fired {
		s.emit(
			sessionevent.Notification{
				Type:       sessionevent.NotifyModelWorking,
				Attributes: map[string]any{workingAttribute: false},
			},
		)
	}

	for _, output := range outputs {
		if output != nil && !output.Existing {
			content := output.LiveContent
			if content == "" {
				content = output.Content
			}

			if content != "" {
				s.emit(sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: content})
			}
		}
	}
}

// buildSessionStatus reports the compaction trigger's own projection and the
// lifetime tree-sum. A backward usage scan would read 0% right after a compaction.
func (s *Session) buildSessionStatus(ctx context.Context) sessionStatus {
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
func (s *Session) openingTurn(prompt string) []llmwire.Message {
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

	return append(msgs, llmwire.Message{Role: llmwire.RoleUser, Content: s.stamper.Stamp(prompt)})
}

func (s *Session) finishRun(ctx context.Context, r *runState, runErr error) (RunResult, error) {
	if r.iterations >= hardIterationCeiling && runErr == nil {
		runErr = fmt.Errorf("maximum iterations (%d) reached", hardIterationCeiling)
	}

	r.result.DeferNoticeAnnounced = s.compactionDeferAnnounced

	r.result.BudgetFired = r.result.BudgetFired || s.budgetFired
	if ctx.Err() != nil {
		return r.result, s.cancelRunActivation(ctx, runErr)
	}

	if err := s.expireActivation(ctx, r.result.Suspended); err != nil {
		runErr = errors.Join(runErr, err)
	}

	if !r.terminalState {
		runErr = s.commitRunState(ctx, r, runErr)
	}

	return r.result, runErr
}

func (s *Session) cancelRunActivation(ctx context.Context, runErr error) error {
	grant := s.currentActivation
	if grant == nil || grant.ToolCallID != "" {
		return runErr
	}

	c := s.newCommit()
	c.Activation = &sessionstore.ActivationChange{
		InputID: grant.InputID, State: sessionstore.ActivationExpired,
		ToolID: grant.ToolID, Command: grant.Command,
	}

	_, err := s.store.Commit(context.WithoutCancel(ctx), c)
	if err != nil && !errors.Is(err, sessionstore.ErrSessionStopping) {
		return errors.Join(runErr, err)
	}

	return runErr
}

func (s *Session) commitRunState(ctx context.Context, r *runState, runErr error) error {
	status := s.runStatus(r, runErr)
	iteration := s.iterationOffset + r.iterations

	todoData, err := json.Marshal(s.prompt.Todos.List())
	if err != nil {
		return errors.Join(runErr, err)
	}

	raw := json.RawMessage(todoData)
	c := s.newCommit()
	c.State = sessionstore.StatePatch{Status: &status, Iteration: &iteration, TodoItems: &raw}

	if runErr != nil {
		if r.result.ErrorNotice == "" {
			r.result.ErrorNotice = projectionErrorNotice(runErr)
		}

		c.Outputs = []sessionstore.Output{{
			Type: sessionstore.OutputMessagePersistent, Content: r.result.ErrorNotice,
			Key: fmt.Sprintf("run:%d:error", iteration), MessageRef: -1, ReleasesInput: true,
		}}
	}

	_, err = s.commit(ctx, c)
	if err != nil {
		return errors.Join(runErr, err)
	}

	return runErr
}

func (s *Session) runStatus(r *runState, runErr error) sessionstore.SessionStatus {
	if runErr != nil {
		return sessionstore.SessionStatusError
	}

	if s.preserveStopped {
		return sessionstore.SessionStatusStopped
	}

	if r.result.Suspended || r.result.BudgetFired {
		return sessionstore.SessionStatusSuspended
	}

	return sessionstore.SessionStatusCompleted
}

func (s *Session) startHeartbeat(ctx context.Context) func() {
	ticker := time.NewTicker(time.Second)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				s.emit(sessionevent.Notification{Type: sessionevent.NotifyHeartbeat})
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		close(done)
		ticker.Stop()
	}
}
