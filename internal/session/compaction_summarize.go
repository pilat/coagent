package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
)

var errCompactionTooLarge = errors.New("conversation does not fit the compaction model's context window")

// compactInitialLocked builds a brief from scratch in one call. A summary that
// never arrived is an error, never a placeholder or a partial summary.
func (s *svc) compactInitialLocked(
	ctx context.Context,
	messages []llmwire.Message,
	acc *compactionUsage,
) (string, error) {
	brief, err := s.compactWithRetry(ctx, acc, func() string {
		return buildInitialPrompt(messages) + s.focusSection()
	})
	if err != nil {
		return "", fmt.Errorf("summarize conversation: %w", err)
	}

	return brief, nil
}

// compactionInputBudget is the window minus writing room, NOT the trigger
// fraction: budgeting by that would refuse every compaction the auto path asks for.
func (s *svc) compactionInputBudget() int {
	return s.contextWindow() - compactionOutputReserve
}

func buildInitialPrompt(messages []llmwire.Message) string {
	var prompt strings.Builder

	prompt.WriteString(registry.CompactionInitialPrompt)
	prompt.WriteString("\n\nConversation:\n\n")

	for _, msg := range messages {
		fmt.Fprintf(&prompt, "[%s]: %s\n\n", msg.Role, msg.Content)
	}

	return prompt.String()
}

// compactMergeLocked folds new messages into an existing brief. A failed merge
// aborts: the old brief would compact those messages away undescribed.
func (s *svc) compactMergeLocked(
	ctx context.Context,
	existingBrief string,
	messages []llmwire.Message,
	acc *compactionUsage,
) (string, error) {
	brief, err := s.compactWithRetry(ctx, acc, func() string {
		mergePrompt := strings.Replace(registry.CompactionMergePrompt, "%s", existingBrief, 1)

		var prompt strings.Builder

		prompt.WriteString(mergePrompt)
		prompt.WriteString("\n\nNew conversation to merge:\n\n")

		for _, msg := range messages {
			fmt.Fprintf(&prompt, "[%s]: %s\n\n", msg.Role, msg.Content)
		}

		prompt.WriteString(s.focusSection())

		return prompt.String()
	})

	if err == nil {
		return brief, nil
	}

	return "", fmt.Errorf("merge %d new messages into the brief: %w", len(messages), err)
}

func validateSummary(brief string) (bool, []string) {
	required := []string{"## Goal", "## Progress", "## Context for Continuation"}

	var missing []string

	for _, section := range required {
		if !strings.Contains(brief, section) {
			missing = append(missing, section)
		}
	}

	return len(missing) == 0, missing
}

// compactWithRetry retries up to 3 times on a summary missing required sections.
// Every call lands in acc, so the summary row carries compaction's own cost.
func (s *svc) compactWithRetry(ctx context.Context, acc *compactionUsage, promptBuilder func() string) (string, error) {
	const maxAttempts = 3

	log := logger.Ctx(ctx).Named("session.compaction")

	var lastBrief string
	var lastMissing []string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		prompt := promptBuilder()

		if attempt > 1 {
			prompt += fmt.Sprintf(
				"\n\nIMPORTANT: Your previous summary was missing these required sections: %s. You MUST include ALL of them: ## Goal, ## Progress, ## Context for Continuation.",
				strings.Join(lastMissing, ", "),
			)
		}

		brief, err := s.compactOnce(ctx, acc, prompt)
		if err != nil {
			return "", err
		}

		lastBrief = brief

		ok, missing := validateSummary(lastBrief)

		if ok {
			return lastBrief, nil
		}

		lastMissing = missing
		log.Info("compaction_quality_gate_retry",
			zap.Int("attempt", attempt),
			zap.Strings("missing_sections", missing),
		)
	}

	log.Warn("compaction_quality_gate_failed",
		zap.Strings("missing_sections", lastMissing),
	)

	return lastBrief, nil
}

// compactOnce runs one summarization call, refusing a prompt the model cannot
// hold rather than silently trimming it into one that fits.
func (s *svc) compactOnce(ctx context.Context, acc *compactionUsage, prompt string) (string, error) {
	system := s.prompt.systemPrompt()

	budget := s.compactionInputBudget()
	if size := estimateText(prompt) + estimateText(system); size > budget {
		return "", fmt.Errorf("%w: ~%d tokens into a %d token budget", errCompactionTooLarge, size, budget)
	}

	// On the call itself, so it bounds the first brief and every later merge alike.
	resp, err := s.chat(ctx, system, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: prompt},
	}, nil, llmwire.WithMaxTokens(compactionOutputReserve))
	if err != nil {
		return "", fmt.Errorf("compaction chat: %w", err)
	}

	acc.add(resp)

	return resp.Text, nil
}
