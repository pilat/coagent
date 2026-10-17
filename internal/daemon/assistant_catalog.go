package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type catalogSelection struct {
	records  map[int64]*sessionstore.SessionRecord
	refs     []assistant.WorkReference
	notes    []assistant.Note
	lastTurn time.Time
	eventIDs []int64
}

func (a *assistantSessions) Catalog(
	ctx context.Context,
	lastTurn time.Time,
	eventIDs ...int64,
) ([]assistant.Project, error) {
	projects, err := a.catalogProjects(ctx)
	if err != nil {
		return nil, err
	}

	refs, err := a.daemon.assistantStore.WorkReferences(ctx, a.root)
	if err != nil {
		return nil, fmt.Errorf("load catalog work references: %w", err)
	}

	notes, err := a.daemon.assistantStore.Notes(ctx, a.root)
	if err != nil {
		return nil, fmt.Errorf("load catalog notes: %w", err)
	}

	records, err := a.daemon.store.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list catalog roots: %w", err)
	}

	selection := catalogSelection{
		records:  map[int64]*sessionstore.SessionRecord{},
		refs:     refs,
		notes:    notes,
		lastTurn: lastTurn,
		eventIDs: eventIDs,
	}
	for _, record := range records {
		selection.records[record.ID] = record
	}

	for i := range projects {
		if err := a.populateCatalogProject(ctx, &projects[i], selection); err != nil {
			return nil, err
		}
	}

	slices.SortFunc(projects, func(a, b assistant.Project) int {
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}

		return strings.Compare(a.Key, b.Key)
	})

	return projects, nil
}

func (a *assistantSessions) populateCatalogProject(
	ctx context.Context,
	p *assistant.Project,
	selection catalogSelection,
) error {
	for _, record := range selection.records {
		if record.ProjectID == p.ProjectID && record.UpdatedAt.After(p.Activity) {
			p.Activity = record.UpdatedAt
		}
	}

	for _, ref := range selection.refs {
		record := selection.records[ref.ID]
		if record == nil || record.ProjectID != p.ProjectID || !a.catalogRoot(record) {
			continue
		}

		activity := record.UpdatedAt
		if ref.LastFinalAt.After(activity) {
			activity = ref.LastFinalAt
		}

		p.SessionIDs = append(p.SessionIDs, record.ID)
		p.Sessions = append(
			p.Sessions,
			assistant.Card{
				ID:       record.ID,
				Activity: activity,
				Pending:  ref.Pending,
				Unread:   ref.LastFinalAt.After(selection.lastTurn),
			},
		)
	}

	p.Sessions = assistant.SelectSessions(p.Sessions, selection.notes, selection.eventIDs)
	for index, candidate := range p.Sessions {
		card, err := a.card(ctx, selection.records[candidate.ID], selection.lastTurn)
		if err != nil {
			return err
		}

		card.ProjectKey, card.Place, card.Pending = p.Key, p.Place, candidate.Pending
		p.Sessions[index] = card
	}

	return nil
}

func (a *assistantSessions) catalogRoot(record *sessionstore.SessionRecord) bool {
	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return owner == a.owner && record.ParentID == 0 && record.KilledAt == nil &&
		record.Status != sessionstore.SessionStatusKilled &&
		record.AgentType != tool.AssistantAgentType &&
		!isManagementSurface(record.Attributes)
}

func (a *assistantSessions) catalogProjects(ctx context.Context) ([]assistant.Project, error) {
	rows, err := a.daemon.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list catalog projects: %w", err)
	}

	worktrees, err := coagenthome.Join(coagenthome.WorktreesDirName)
	if err != nil {
		return nil, fmt.Errorf("resolve catalog worktrees: %w", err)
	}
	var projects []assistant.Project
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

		projects = append(
			projects,
			assistant.Project{ProjectID: row.ID, Name: row.Name, Key: row.Name, Place: projectPlace(row.WorkDir)},
		)
		names[row.Name]++
		paths[row.ID] = row.WorkDir
	}

	for i := range projects {
		if names[projects[i].Name] > 1 {
			projects[i].Key = catalogPath(paths[projects[i].ProjectID])
			if projects[i].Place == "scratch" {
				projects[i].Place += " " + projects[i].Key
			}
		}
	}

	return projects, nil
}

func (a *assistantSessions) catalogProjectKey(ctx context.Context, id int64, fallback string) (string, error) {
	projects, err := a.catalogProjects(ctx)
	if err != nil {
		return "", err
	}

	for _, project := range projects {
		if project.ProjectID == id {
			return project.Key, nil
		}
	}

	return fallback, nil
}

func catalogPath(path string) string {
	if home, err := coagenthome.UserHome(); err == nil && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}

	return path
}
