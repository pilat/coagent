package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	assistantWorking = "working"
	// Below the event limit, so a read observation is never cut in the middle.
	readBudget = 18000
)

func (a *assistantSessions) Read(ctx context.Context, id int64, count int) (string, error) {
	if _, err := a.workRoot(ctx, id); err != nil {
		return "", err
	}

	ids, err := a.daemon.store.AcceptedUserMessageIDs(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load assistant read inputs: %w", err)
	}

	messages, err := a.daemon.store.LoadActiveMessages(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load assistant read transcript: %w", err)
	}

	var entries []string

	for _, message := range messages {
		if (message.Role == "user" && slices.Contains(ids, message.ID)) ||
			sessionstore.IsFinalAssistantMessage(message) {
			entries = append(entries, message.Role+": "+strings.TrimSpace(message.Content))
		}
	}

	return newestWithin(entries[max(0, len(entries)-count):], readBudget), nil
}

func (a *assistantSessions) card(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	lastTurn time.Time,
) (assistant.Card, error) {
	name, err := a.daemon.store.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant card project: %w", err)
	}

	workDir, err := a.daemon.store.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant card place: %w", err)
	}

	asked, err := a.daemon.store.LastUserInput(ctx, record.ID)
	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant card request: %w", err)
	}

	card := assistant.Card{
		ID: record.ID, Project: name, Place: projectPlace(workDir), Activity: record.UpdatedAt,
		Status:        assistantCardStatus(record.Status, a.daemon.HasActiveLoop(record.ID)),
		Asked:         strings.Join(strings.Fields(asked), " "),
		ContextTokens: record.ContextBaselinePromptTokens,
	}

	card.CostUSD, err = a.daemon.store.SessionTreeCost(ctx, record.ID)
	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant card cost: %w", err)
	}

	output, err := a.daemon.store.LatestModelOutput(ctx, record.ID)
	if errors.Is(err, sessionstore.ErrNoOutput) {
		return card, nil
	}

	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant card output: %w", err)
	}

	card.LastLine = assistant.Clip(withoutFooter(output.Content), 200)
	card.Unread = output.CreatedAt.After(lastTurn)
	card.LastFinalAt = output.CreatedAt

	if output.CreatedAt.After(card.Activity) {
		card.Activity = output.CreatedAt
	}

	return card, nil
}

func assistantCardStatus(status sessionstore.SessionStatus, running bool) string {
	if running {
		return assistantWorking
	}

	switch status {
	case sessionstore.SessionStatusActive, sessionstore.SessionStatusSuspended:
		return assistantWorking
	case sessionstore.SessionStatusCompleted:
		return "done"
	case sessionstore.SessionStatusError:
		return assistantFailed
	case sessionstore.SessionStatusStopping, sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusTerminating, sessionstore.SessionStatusKilled:
		return string(sessionstore.SessionStatusStopped)
	default:
		return string(sessionstore.SessionStatusStopped)
	}
}

func withoutFooter(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "🟣 Background") || strings.HasPrefix(line, "🤖") {
			return strings.TrimSpace(strings.Join(lines[:i], "\n"))
		}
	}

	return strings.TrimSpace(text)
}

// newestWithin keeps whole entries from the newest backwards; only a single oversized
// entry is cut, and then in its middle.
func newestWithin(entries []string, budget int) string {
	var kept []string

	used := 0

	for _, entry := range slices.Backward(entries) {
		size := utf8.RuneCountInString(entry) + 2
		if used+size > budget {
			if len(kept) == 0 {
				kept = append(kept, assistant.ClipMiddle(entry, budget))
			}

			break
		}

		kept = append(kept, entry)
		used += size
	}

	slices.Reverse(kept)

	return strings.Join(kept, "\n\n")
}

// projectPlace tells the assistant whether a session works in the user's code or in a
// project Coagent made for it, so a related-looking name does not pull work into a repo.
func projectPlace(workDir string) string {
	if projects, err := coagenthome.Join(coagenthome.ProjectsDirName); err == nil &&
		(workDir == projects || strings.HasPrefix(workDir, projects+string(os.PathSeparator))) {
		return "scratch"
	}

	shown := workDir
	if home, err := coagenthome.UserHome(); err == nil && strings.HasPrefix(workDir, home+string(os.PathSeparator)) {
		shown = "~" + strings.TrimPrefix(workDir, home)
	}

	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		return "repo " + shown
	}

	return "dir " + shown
}
