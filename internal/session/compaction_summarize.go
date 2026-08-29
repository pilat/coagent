package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

// summarizeCheckpoint runs the one no-tools model call that produces the
// checkpoint: one canonical text input, one accepted completed non-empty text
// output. No compaction-specific retry exists — a failed attempt persists no
// boundary and may submit the same head again on a later attempt.
func (s *svc) summarizeCheckpoint(
	ctx context.Context,
	headerJSONL, prevSummary, headJSONL string,
	window int,
) (string, *compactionUsage, error) {
	if s.budgetGate != nil {
		if err := s.budgetGate.Admit(ctx, time.Now().UTC()); err != nil {
			return "", nil, fmt.Errorf("budget admission for compaction: %w", err)
		}
	}

	prompt := buildSummarizerPrompt(headerJSONL, prevSummary, headJSONL, s.focusSection())

	// The 50% bound was enforced by the split selection; this defensive recheck
	// compares the exact final text rather than trusting the selection estimate.
	if size := estimateText(s.prompt.systemPrompt()) + estimateText(prompt); size > window/2 {
		return "", nil, fmt.Errorf("summarizer request exceeds its half-window bound: ~%d of %d tokens", size, window/2)
	}

	// The normal full output reserve: the ordinary complement of the input
	// fraction, not a summary-length target — any useful completed length passes.
	reserve := int((1 - llmwire.ContextInputFraction) * float64(window))

	resp, err := s.chat(ctx, s.prompt.systemPrompt(), []llmwire.Message{
		{Role: llmwire.RoleUser, Content: prompt},
	}, nil, llmwire.WithMaxTokens(reserve))
	if err != nil {
		return "", nil, fmt.Errorf("compaction chat: %w", err)
	}

	acc := &compactionUsage{}
	acc.add(resp)

	summaryText, err := acceptedCheckpointText(resp)
	if err != nil {
		return "", acc, err
	}

	return summaryText, acc, nil
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

// The static section renderers behind both the request estimate and the actual
// summarizer prompt, so the 50% bound is estimated against the exact bytes.
func headerSection(headerJSONL string) string {
	return "\n\n" + summarizeHeaderSection + " (context only, never summarized):\n" + headerJSONL
}

func prevSummarySection(prevSummary string) string {
	return "\n" + summarizePrevSection +
		" (the running checkpoint anchor; fold the history below into it):\n" + prevSummary + "\n\n"
}

func historySectionHeader() string {
	return summarizeHistorySection + " (JSON Lines, one message per line):\n"
}

// buildSummarizerPrompt renders the one canonical summarizer user message.
// Sections are fixed and ordered; the section markers are static.
func buildSummarizerPrompt(headerJSONL, prevSummary, headJSONL, focus string) string {
	var b strings.Builder

	b.WriteString(registry.CompactionSummaryPrompt)

	if focus != "" {
		b.WriteString(focus)
	}

	b.WriteString(headerSection(headerJSONL))

	if prevSummary != "" {
		b.WriteString(prevSummarySection(prevSummary))
	}

	b.WriteString(historySectionHeader())
	b.WriteString(headJSONL)

	return b.String()
}
