package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
)

const (
	mainCalls    = 12
	guardRounds  = 2
	guardCalls   = 4
	roundCapNote = "No more tool calls in this turn: write your reply now."
)

func (l *loop) mainPhase(ctx context.Context, t *turn) error {
	for range mainCalls {
		response, err := l.ask(ctx, t, phaseMain, t.messages)
		if err != nil {
			return err
		}

		keep(t, response, true)

		if len(response.ToolCalls) == 0 {
			return nil
		}

		l.runTools(ctx, t, response.ToolCalls, phaseMain)
	}

	t.messages = append(t.messages, llmwire.Message{Role: llmwire.RoleUser, Content: roundCapNote})

	response, err := l.ask(ctx, t, phaseFinal, t.messages)
	if err != nil {
		return err
	}

	keep(t, response, true)
	ignoreTools(t, response.ToolCalls)

	return nil
}

// consolidate turns several text segments into the single message the user receives, on a branch
// of the conversation so the guard continues from what the model actually did.
func (l *loop) consolidate(ctx context.Context, t *turn) (string, error) {
	if len(t.replies) <= 1 {
		return strings.Join(t.replies, ""), nil
	}

	var b strings.Builder
	b.WriteString("The user receives exactly one message from this turn, and you wrote several pieces of text:\n")

	for i, reply := range t.replies {
		fmt.Fprintf(&b, "\n--- piece %d ---\n%s\n", i+1, reply)
	}

	b.WriteString("\nWrite the single message that replaces them all: only the text for the user, " +
		"nothing about pieces, no tool calls.")

	branch := append(slices.Clone(t.messages), llmwire.Message{Role: llmwire.RoleUser, Content: b.String()})

	response, err := l.ask(ctx, t, phaseConsolidate, branch)
	if ctx.Err() != nil {
		return "", fmt.Errorf("assistant consolidation interrupted: %w", ctx.Err())
	}

	if err != nil || strings.TrimSpace(response.Text) == "" {
		//nolint:nilerr // A failed consolidation falls back to the joined pieces.
		return strings.Join(t.replies, "\n\n"), nil
	}

	return strings.TrimSpace(response.Text), nil
}

// guard never fails a turn: an unresolved check commits with the fixed reply and a warning.
func (l *loop) guard(ctx context.Context, t *turn) (string, error) {
	log := logger.Ctx(ctx).Named("assistant.guard")

	for range guardRounds {
		problems := t.problems()
		if len(problems) == 0 {
			return "ok", nil
		}

		t.messages = append(t.messages, llmwire.Message{Role: llmwire.RoleUser, Content: guardNote(problems)})

		for call := 1; call <= guardCalls; call++ {
			response, err := l.ask(ctx, t, phaseGuard, t.messages)
			if ctx.Err() != nil {
				return "", fmt.Errorf("assistant guard interrupted: %w", ctx.Err())
			}

			if err != nil {
				log.Warn("guard call failed; committing anyway", zap.Error(err))
				return "guard_unresolved", nil
			}

			keep(t, response, false)

			if len(response.ToolCalls) == 0 {
				break
			}

			if call == guardCalls {
				ignoreTools(t, response.ToolCalls)
				break
			}

			l.runTools(ctx, t, response.ToolCalls, phaseGuard)
		}
	}

	if problems := t.problems(); len(problems) > 0 {
		log.Warn("guard unresolved; committing anyway", zap.Strings("problems", problems))
		return "guard_unresolved", nil
	}

	return "ok", nil
}

func (l *loop) ask(ctx context.Context, t *turn, phase string, messages []llmwire.Message) (*llmwire.Response, error) {
	call := Call{
		SessionID: l.sessionID, InputID: t.input.ID, Number: len(t.calls) + 1, Phase: phase,
		Request: callRequest(messages[min(t.recorded, len(messages)):]), CreatedAt: l.deps.Now(),
	}

	response, err := l.chat(ctx, messages)
	if err != nil {
		call.Response = "error: " + logger.Redact(err.Error())
		t.calls = append(t.calls, call)

		return nil, err
	}

	call.Response = callResponse(response)
	t.calls = append(t.calls, call)

	return response, nil
}

func (l *loop) chat(ctx context.Context, messages []llmwire.Message) (*llmwire.Response, error) {
	var last error
	delays := []time.Duration{0, 2 * time.Second, 5 * time.Second, 15 * time.Second}

	for _, delay := range delays {
		if err := waitRetry(ctx, delay); err != nil {
			return nil, err
		}

		response, err := l.chatOnce(ctx, messages)
		if err == nil {
			return response, nil
		}

		last = err
	}

	return nil, fmt.Errorf("assistant model retries exhausted: %w", last)
}

// chatOnce sends the same tools on every call so the provider's prompt cache keeps hitting.
func (l *loop) chatOnce(ctx context.Context, messages []llmwire.Message) (*llmwire.Response, error) {
	log := logger.Ctx(ctx).Named("assistant.chat")

	record, err := l.deps.Store.GetSession(ctx, l.sessionID)
	if err != nil {
		return nil, fmt.Errorf("load assistant model: %w", err)
	}

	client, err := l.deps.NewClient(record.Model)
	if err != nil {
		return nil, fmt.Errorf("build assistant model: %w", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn("close model", zap.Error(err))
		}
	}()

	response, err := client.Chat(ctx, registry.AssistantPrompt, messages, Tools())
	if err != nil {
		return nil, fmt.Errorf("call assistant model: %w", err)
	}

	// A cut-off or filtered reply must not reach the user as if it were whole.
	if response.FinishType == llmwire.FinishLength || response.FinishType == llmwire.FinishUnknown {
		return nil, fmt.Errorf("assistant model stopped early: %s %s", response.FinishType,
			response.ProviderFinishReason)
	}

	log.Info("call usage", zap.Any("usage", response.Usage), zap.Float64("cost_usd", response.CostUSD))

	return response, nil
}

// keep replays the response, reasoning included, into the turn conversation; only main-phase text can be spoken.
// Text is replayed trimmed because providers reject whitespace-only text blocks; missing call ids get stand-ins.
func keep(t *turn, response *llmwire.Response, speak bool) {
	text := strings.TrimSpace(response.Text)

	for i := range response.ToolCalls {
		if response.ToolCalls[i].ID == "" {
			response.ToolCalls[i].ID = fmt.Sprintf("call_%d_%d", len(t.calls), i)
		}
	}

	t.messages = append(t.messages, llmwire.Message{
		Role: llmwire.RoleAssistant, Content: text, ToolCalls: response.ToolCalls,
		ReasoningContent: response.ReasoningContent, ReasoningRaw: response.ReasoningRaw,
	})
	t.recorded = len(t.messages)

	if speak && text != "" {
		t.replies = append(t.replies, text)
	}
}

func guardNote(problems []string) string {
	var b strings.Builder
	b.WriteString("Before this turn is committed, fix your bookkeeping with the memory and focus tools. " +
		"Your reply is already final and is not changed by anything you write now; write no text for the user.\n")

	for _, problem := range problems {
		b.WriteString("- " + problem + "\n")
	}

	return b.String()
}

func callRequest(messages []llmwire.Message) string {
	type entry struct {
		Role    string `json:"role"`
		Tool    string `json:"tool,omitempty"`
		Content string `json:"content"`
	}

	entries := make([]entry, 0, len(messages))
	for _, message := range messages {
		entries = append(entries, entry{Role: message.Role, Tool: message.ToolName, Content: message.Content})
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Sprintf("unencodable request: %v", err)
	}

	return string(data)
}

func callResponse(response *llmwire.Response) string {
	type call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}

	calls := make([]call, 0, len(response.ToolCalls))
	for _, tc := range response.ToolCalls {
		args := json.RawMessage(tc.Arguments)
		if !json.Valid(args) {
			args, _ = json.Marshal(string(tc.Arguments)) //nolint:errchkjson // a Go string always encodes.
		}

		calls = append(calls, call{Name: tc.Name, Arguments: args})
	}

	data, err := json.Marshal(struct {
		Text  string `json:"text"`
		Calls []call `json:"tool_calls,omitempty"`
	}{response.Text, calls})
	if err != nil {
		return fmt.Sprintf("unencodable response: %v", err)
	}

	return string(data)
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("assistant retry interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
