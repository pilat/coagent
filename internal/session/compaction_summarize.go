package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// summarizeCheckpoint runs the one no-tools model call that produces the
// checkpoint: the ordinary repaired prefix from the transcript beginning
// through the split, the same system prompt, schemas and tool-choice behavior
// an ordinary request carries, and one final role-user instruction. A model
// that answers a tool call instead of text gets one tools-unavailable nudge
// and must then answer in plain text. A failed attempt persists no boundary
// and may submit the same head again on a later attempt.
func (s *svc) summarizeCheckpoint(
	ctx context.Context,
	split int,
	pendingExternal map[string]bool,
	window int,
) (string, *compactionUsage, error) {
	if s.budgetGate != nil {
		if err := s.budgetGate.Admit(ctx, time.Now().UTC()); err != nil {
			return "", nil, fmt.Errorf("budget admission for compaction: %w", err)
		}
	}

	activeTools := s.registry.List()
	if s.loopDetector.forceTextOnly {
		activeTools = nil
	}

	schemas := tool.ToSchemas(activeTools)

	messages := append(
		repairTranscriptExcluding(s.ms.messages[:split], pendingExternal),
		compactionInstructionMessage(s.focusSection()),
	)

	// The normal full output reserve: the ordinary complement of the input
	// fraction, not a summary-length target — any useful completed length passes.
	reserve := int((1 - llmwire.ContextInputFraction) * float64(window))

	resp, err := s.chat(ctx, s.prompt.systemPrompt(), messages, schemas, llmwire.WithMaxTokens(reserve))
	if err != nil {
		return "", nil, fmt.Errorf("compaction chat: %w", err)
	}

	acc := &compactionUsage{}
	acc.add(resp)

	if len(resp.ToolCalls) > 0 {
		retry, err := s.rejectSummarizerToolCall(ctx, messages, schemas, reserve, resp)
		if err != nil {
			return "", acc, err
		}

		acc.add(retry)
		resp = retry
	}

	summaryText, err := acceptedCheckpointText(resp)
	if err != nil {
		return "", acc, err
	}

	return summaryText, acc, nil
}

// rejectSummarizerToolCall answers a tool-calling summarizer once, in role:
// the call keeps its recorded tool results, so the transcript stays provider-
// valid, and the demand to summarize is restated. One nudge only.
func (s *svc) rejectSummarizerToolCall(
	ctx context.Context,
	messages []llmwire.Message,
	schemas []llmwire.ToolSchema,
	reserve int,
	resp *llmwire.Response,
) (*llmwire.Response, error) {
	replies := make([]llmwire.Message, 0, len(resp.ToolCalls))

	for _, tc := range resp.ToolCalls {
		replies = append(replies, llmwire.Message{
			Role:       llmwire.RoleTool,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    "TOOLS ARE UNAVAILABLE. Do not call any tools. I am waiting for the summary text right now.",
		})
	}

	followUp := append(append([]llmwire.Message{}, messages...), replies...)

	retry, err := s.chat(ctx, s.prompt.systemPrompt(), followUp, schemas, llmwire.WithMaxTokens(reserve))
	if err != nil {
		return nil, fmt.Errorf("compaction retry after tool call: %w", err)
	}

	return retry, nil
}

// acceptedCheckpointText validates the single accepted shape: one fully
// completed, non-empty text response with no tool calls. Missing headings or a
// short answer are fine; anything else is not a checkpoint.
func acceptedCheckpointText(resp *llmwire.Response) (string, error) {
	if resp == nil {
		return "", errors.New("empty summarizer response")
	}

	if len(resp.ToolCalls) > 0 {
		return "", errors.New("summarizer attempted tool calls")
	}

	switch resp.FinishType {
	case llmwire.FinishStop:
	case llmwire.FinishLength:
		return "", errors.New("summarizer output stopped for length")
	default:
		return "", fmt.Errorf("summarizer finished with %q, not a normal completion", resp.FinishType)
	}

	if strings.TrimSpace(resp.Text) == "" {
		return "", errors.New("summarizer returned no text")
	}

	return strings.TrimSpace(resp.Text), nil
}

// compactionInstructionMessage renders the final role-user instruction:
// the revised checkpoint prompt plus the optional /compact focus.
func compactionInstructionMessage(focus string) llmwire.Message {
	content := registry.CompactionSummaryPrompt + focus

	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}
