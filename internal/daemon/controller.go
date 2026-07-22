package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
)

var _ controllerapi.Controller = (*controller)(nil)

type controller struct {
	svc      Service
	cfg      *config.Config
	cache    loader.MarketplaceCache
	schedule schedule.Service
}

func NewController(
	svc Service,
	cfg *config.Config,
	cache loader.MarketplaceCache,
	scheduleSvc schedule.Service,
) controllerapi.Controller {
	return &controller{svc: svc, cfg: cfg, cache: cache, schedule: scheduleSvc}
}

// ListSchedules returns the schedules attached to a session for read-only
// display (Telegram /schedules). Mutations stay with the agent's schedule tool.
func (c *controller) ListSchedules(
	ctx context.Context,
	data controllerapi.ScheduleListData,
) (*controllerapi.ScheduleListResultData, error) {
	if c.schedule == nil {
		return &controllerapi.ScheduleListResultData{}, nil
	}

	entries, err := c.schedule.ListSchedules(ctx, data.SessionID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}

	schedules := make([]controllerapi.ScheduleInfo, len(entries))
	for i, e := range entries {
		info := controllerapi.ScheduleInfo{
			ID:          e.ID(),
			OneShotAt:   e.OneShotAt(),
			LastFiredAt: e.LastFiredAt(),
			Fresh:       e.Fresh(),
			Prompt:      e.InputMessage(),
		}

		if cronExpr := e.CronExpr(); cronExpr != "" {
			info.Cron, info.Timezone = schedule.SplitCronTZ(cronExpr)
		}

		schedules[i] = info
	}

	return &controllerapi.ScheduleListResultData{Schedules: schedules}, nil
}

func (c *controller) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	if data.UseWorktree {
		nextWorkDir, err := createWorktree(ctx, data.WorkDir)
		if err != nil {
			return 0, err
		}

		data.WorkDir = nextWorkDir
	}

	projectID, err := c.svc.GetOrCreateProject(ctx, data.WorkDir)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	sessionID, err := c.svc.Send(ctx, projectID, data.Prompt, data.Model, data.Attributes)
	if err != nil {
		return 0, fmt.Errorf("send session: %w", err)
	}

	if data.Prompt != "" {
		c.publishInputReceived(sessionID, data.Prompt, "user")
	}

	return sessionID, nil
}

func (c *controller) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	if err := c.svc.SendToSession(ctx, data.SessionID, data.Message); err != nil {
		return fmt.Errorf("send session message: %w", err)
	}

	if data.Message != "" {
		c.publishInputReceived(data.SessionID, data.Message, "user")
	}

	return nil
}

func (c *controller) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	records, err := c.svc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	infos := make([]controllerapi.SessionInfo, 0, len(records))

	for _, rec := range records {
		workDir, _ := c.svc.GetProjectWorkDir(ctx, rec.ProjectID)
		projectName, _ := c.svc.GetProjectName(ctx, rec.ProjectID)
		name := fmt.Sprintf("%s - %d", projectName, rec.ID)
		infos = append(infos, controllerapi.SessionInfo{
			ID:             rec.ID,
			Name:           name,
			WorkDir:        workDir,
			ProjectID:      rec.ProjectID,
			HasActiveLoop:  c.svc.HasActiveLoop(rec.ID),
			Model:          rec.Model,
			ReasoningLevel: rec.ReasoningLevel,
			Status:         string(rec.Status),
			Attributes:     rec.Attributes,
			UpdatedAt:      rec.UpdatedAt,
			KilledAt:       rec.KilledAt,
		})
	}

	return infos, nil
}

func (c *controller) KillSession(ctx context.Context, data controllerapi.SessionKillData) error {
	if err := c.svc.Kill(ctx, data.SessionID); err != nil {
		return fmt.Errorf("kill session: %w", err)
	}

	return nil
}

func (c *controller) StopSession(ctx context.Context, data controllerapi.SessionStopData) error {
	if err := c.svc.Stop(ctx, data.SessionID); err != nil {
		return fmt.Errorf("stop session: %w", err)
	}

	return nil
}

func (c *controller) ClearSession(ctx context.Context, data controllerapi.SessionClearData) (int64, error) {
	id, err := c.svc.Clear(ctx, data.SessionID)
	if err != nil {
		return 0, fmt.Errorf("clear session: %w", err)
	}

	return id, nil
}

func (c *controller) SetSessionModel(ctx context.Context, data controllerapi.SessionSetModelData) error {
	if err := c.svc.SetModel(ctx, data.SessionID, data.Model, data.ReasoningLevel); err != nil {
		return fmt.Errorf("set session model: %w", err)
	}

	return nil
}

func (c *controller) SetSessionAttributes(ctx context.Context, data controllerapi.SessionSetAttributesData) error {
	if err := c.svc.SetAttributes(ctx, data.SessionID, data.Attributes); err != nil {
		return fmt.Errorf("set session attributes: %w", err)
	}

	return nil
}

func (c *controller) ListDir(
	_ context.Context,
	data controllerapi.FsListDirData,
) (*controllerapi.FsListDirResultData, error) {
	return c.listDir(data)
}

func (c *controller) ListModels(context.Context) (*controllerapi.ConfigModelsResultData, error) {
	var models []controllerapi.ConfigModelInfo

	var defaultID string

	if uc := c.cfg.UnifiedConfig; uc != nil {
		for _, m := range uc.Models {
			info := controllerapi.ConfigModelInfo{
				ID:            m.ID,
				Name:          m.Name,
				DisplayName:   m.DisplayName,
				EffortLevels:  m.EffortLevels,
				DefaultEffort: m.DefaultEffort,
			}

			if m.Pricing != nil {
				info.InputPrice = m.Pricing.InputPrice
				info.OutputPrice = m.Pricing.OutputPrice
			}

			models = append(models, info)
		}
	}

	if len(models) > 0 {
		defaultID = models[0].ID
	}

	return &controllerapi.ConfigModelsResultData{
		Models:    models,
		DefaultID: defaultID,
	}, nil
}

func (c *controller) ListSkills(
	ctx context.Context,
	data controllerapi.ConfigSkillsData,
) (*controllerapi.ConfigSkillsResultData, error) {
	workDir, err := c.skillContext(ctx, data.SessionID)
	if err != nil {
		return nil, err
	}

	ldr := loader.New(c.cache)

	if uc := c.cfg.UnifiedConfig; uc != nil && len(uc.Marketplaces) > 0 {
		ldr.ProcessMarketplaces(ctx, uc.Marketplaces, nil)
	}

	_ = ldr.LoadSkills(workDir)

	list := ldr.ListUserInvocableSkills()
	skills := make([]controllerapi.ConfigSkillInfo, len(list))

	for i, sk := range list {
		skills[i] = controllerapi.ConfigSkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
		}
	}

	return &controllerapi.ConfigSkillsResultData{Skills: skills}, nil
}

func (c *controller) SubscribeAll() <-chan controllerapi.SessionNotification {
	return c.svc.PubSub().SubscribeAll()
}

func (c *controller) UnsubscribeAll(ch <-chan controllerapi.SessionNotification) {
	c.svc.PubSub().UnsubscribeAll(ch)
}

func (c *controller) skillContext(ctx context.Context, sessionID int64) (string, error) {
	if sessionID == 0 {
		return ".", nil
	}

	record, err := c.svc.GetSession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	workDir, err := c.svc.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil || workDir == "" {
		workDir = "."
	}

	return workDir, nil
}

func (c *controller) listDir(data controllerapi.FsListDirData) (*controllerapi.FsListDirResultData, error) {
	var favorites []string
	if uc := c.cfg.UnifiedConfig; uc != nil {
		favorites = uc.SpawnFavorites
	}

	if favorites == nil {
		favorites = []string{}
	}

	home, homeErr := coagenthome.UserHome()

	path := data.Path
	if path == "" {
		if homeErr != nil {
			return nil, fmt.Errorf("home dir: %w", homeErr)
		}

		path = home
	}

	dirs, err := readSubdirs(path)
	if err != nil {
		return nil, err
	}

	parent := ""
	if path != "/" {
		parent = filepath.Dir(path)
	}

	return &controllerapi.FsListDirResultData{
		Dirs:      dirs,
		Favorites: favorites,
		Home:      home,
		Path:      path,
		Parent:    parent,
	}, nil
}

func (c *controller) publishInputReceived(sessionID int64, message, source string) {
	c.svc.NotifySession(sessionID, sessionevent.Notification{
		Type:    sessionevent.NotifyInputReceived,
		Message: message,
		Source:  source,
	})
}

// readSubdirs returns the visible subdirectories of path, newest mtime first.
func readSubdirs(path string) ([]controllerapi.FsDirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}

	type dirWithMtime struct {
		name  string
		mtime time.Time
	}

	var filtered []dirWithMtime

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == "" || name[0] == '.' {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		filtered = append(filtered, dirWithMtime{name: name, mtime: info.ModTime()})
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].mtime.After(filtered[j].mtime)
	})

	dirs := make([]controllerapi.FsDirEntry, 0, len(filtered))
	for _, d := range filtered {
		dirs = append(dirs, controllerapi.FsDirEntry{Name: d.name, Path: filepath.Join(path, d.name)})
	}

	return dirs, nil
}

func createWorktree(ctx context.Context, workDir string) (string, error) {
	wt := git.NewWorktreeClient()

	gitRoot, err := wt.FindRoot(ctx, workDir)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	worktreePath, fullWorkDir, branchName := git.ComputeWorktreePaths(workDir, gitRoot, time.Now())
	if err := wt.CreateWorktree(ctx, gitRoot, worktreePath, branchName); err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	return fullWorkDir, nil
}
