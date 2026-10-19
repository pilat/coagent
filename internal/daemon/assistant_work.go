package daemon

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	assistantWorking = "working"
	// Below the event limit, so a read is never cut in the middle by the desk.
	readBudget = 18000
)

// projectEntry is a listed project with the id the assistant never sees.
type projectEntry struct {
	assistant.Project
	id int64
}

func (a *assistantSessions) Recent(ctx context.Context, limit int) ([]assistant.Card, int, error) {
	roots, err := a.daemon.assistantStore.WorkRoots(ctx, a.scope(), true)
	if err != nil {
		return nil, 0, fmt.Errorf("list recent assistant work: %w", err)
	}

	sortWorkRoots(roots)
	cards, err := a.cards(ctx, roots[:min(limit, len(roots))])

	return cards, len(roots), err
}

func (a *assistantSessions) List(
	ctx context.Context,
	offset, limit int,
	project string,
) ([]assistant.Card, int, error) {
	roots, err := a.daemon.assistantStore.WorkRoots(ctx, a.scope(), false)
	if err != nil {
		return nil, 0, fmt.Errorf("list assistant work: %w", err)
	}

	if project != "" {
		id, err := a.projectID(ctx, project)
		if err != nil {
			return nil, 0, err
		}

		roots = slices.DeleteFunc(roots, func(root assistant.WorkRoot) bool { return root.ProjectID != id })
	}

	sortWorkRoots(roots)
	offset = min(offset, len(roots))
	page := roots[offset:min(offset+limit, len(roots))]
	cards, err := a.cards(ctx, page)

	return cards, len(roots), err
}

func (a *assistantSessions) Projects(ctx context.Context) ([]assistant.Project, error) {
	entries, err := a.projects(ctx)
	if err != nil {
		return nil, err
	}

	records, err := a.daemon.store.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list project activity: %w", err)
	}

	projects := make([]assistant.Project, 0, len(entries))

	for _, entry := range entries {
		for _, record := range records {
			if record.ProjectID == entry.id && record.UpdatedAt.After(entry.Activity) {
				entry.Activity = record.UpdatedAt
			}
		}

		projects = append(projects, entry.Project)
	}

	slices.SortStableFunc(projects, func(a, b assistant.Project) int {
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		return strings.Compare(a.Key, b.Key)
	})

	return projects, nil
}

func (a *assistantSessions) Search(ctx context.Context, query string) ([]assistant.Hit, error) {
	//nolint:wrapcheck // Store errors are already model-facing tool results.
	return a.daemon.assistantStore.Search(ctx, a.scope(), query)
}

func (a *assistantSessions) Around(ctx context.Context, ref string, count int) (string, error) {
	//nolint:wrapcheck // Store errors are already model-facing tool results.
	return a.daemon.assistantStore.Around(ctx, a.scope(), ref, count)
}

// Read includes compacted messages: an old final is still what the session answered.
func (a *assistantSessions) Read(ctx context.Context, id int64, count int) (string, error) {
	if _, err := a.workRoot(ctx, id); err != nil {
		return "", err
	}

	ids, err := a.daemon.store.AcceptedUserMessageIDs(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load assistant read inputs: %w", err)
	}

	messages, err := a.daemon.store.LoadReadableMessages(ctx, id)
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

	if len(entries) == 0 {
		return "(no requests or answers yet)", nil
	}

	return newestWithin(entries[max(0, len(entries)-count):], readBudget), nil
}

func (a *assistantSessions) Target(ctx context.Context, id int64) (assistant.Card, error) {
	record, err := a.workRoot(ctx, id)
	if err != nil {
		return assistant.Card{}, err
	}

	name, err := a.daemon.store.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return assistant.Card{}, fmt.Errorf("read assistant target project: %w", err)
	}

	return assistant.Card{
		ID: record.ID, Project: name, CreatedAt: record.CreatedAt, Activity: record.UpdatedAt,
		ContextTokens: record.ContextBaselinePromptTokens,
		Status:        assistantCardStatus(record.Status, a.daemon.HasActiveLoop(record.ID)),
	}, nil
}

func (a *assistantSessions) scope() assistant.Scope {
	return assistant.Scope{Root: a.root, Owner: a.owner}
}

func (a *assistantSessions) cards(ctx context.Context, roots []assistant.WorkRoot) ([]assistant.Card, error) {
	rows, err := a.daemon.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list assistant card projects: %w", err)
	}

	names := make(map[int64]string, len(rows))
	for _, row := range rows {
		names[row.ID] = row.Name
	}

	cards := make([]assistant.Card, 0, len(roots))

	for _, root := range roots {
		cards = append(cards, assistant.Card{
			ID:        root.ID,
			Project:   names[root.ProjectID],
			CreatedAt: root.CreatedAt,
			Activity:  root.Activity,
			Status: assistantCardStatus(
				sessionstore.SessionStatus(root.Status),
				a.daemon.HasActiveLoop(root.ID),
			),
			ContextTokens: root.Context,
			Pending:       root.Pending,
			About:         root.About,
		})
	}

	return cards, nil
}

// projects excludes hidden projects, missing folders and worktrees; a name shared by several uses its path as key.
func (a *assistantSessions) projects(ctx context.Context) ([]projectEntry, error) {
	rows, err := a.daemon.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list assistant projects: %w", err)
	}

	worktrees, err := coagenthome.Join(coagenthome.WorktreesDirName)
	if err != nil {
		return nil, fmt.Errorf("resolve assistant worktrees: %w", err)
	}

	var entries []projectEntry

	names := map[string]int{}
	paths := map[int64]string{}

	for _, row := range rows {
		if row.Hidden || row.WorkDir == worktrees ||
			strings.HasPrefix(row.WorkDir, worktrees+string(filepath.Separator)) {
			continue
		}

		info, err := os.Stat(row.WorkDir)
		if err != nil || !info.IsDir() {
			continue
		}

		entries = append(entries, projectEntry{
			Name: row.Name, Key: row.Name, Place: projectPlace(row.WorkDir), id: row.ID,
		})
		names[row.Name]++
		paths[row.ID] = row.WorkDir
	}

	for i := range entries {
		if names[entries[i].Name] > 1 {
			entries[i].Key = catalogPath(paths[entries[i].id])
			if entries[i].Place == "scratch" {
				entries[i].Place += " " + entries[i].Key
			}
		}
	}

	return entries, nil
}

func (a *assistantSessions) projectID(ctx context.Context, key string) (int64, error) {
	entries, err := a.projects(ctx)
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		if entry.Key == key {
			return entry.id, nil
		}
	}

	return 0, fmt.Errorf("no project %q: projects lists the keys", key)
}

func sortWorkRoots(roots []assistant.WorkRoot) {
	slices.SortStableFunc(roots, func(a, b assistant.WorkRoot) int {
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		return cmp.Compare(b.ID, a.ID)
	})
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

	shown := catalogPath(workDir)
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		return "repo " + shown
	}

	return "dir " + shown
}

func catalogPath(path string) string {
	if home, err := coagenthome.UserHome(); err == nil && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}

	return path
}
