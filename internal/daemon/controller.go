//nolint:wrapcheck // Bound controller methods preserve capability-domain errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

var (
	_ controllerapi.Controller               = (*controller)(nil)
	_ controllerapi.ManagerControllerFactory = (*controller)(nil)
)

type controller struct {
	svc       Service
	cfg       *config.Config
	cache     loader.MarketplaceCache
	schedule  schedule.Service
	managerID string
}

type outputStoreSource interface {
	OutputStore() sessionstore.OutputStore
}

func NewController(
	svc Service,
	cfg *config.Config,
	cache loader.MarketplaceCache,
	scheduleSvc schedule.Service,
) controllerapi.ManagerControllerFactory {
	return &controller{svc: svc, cfg: cfg, cache: cache, schedule: scheduleSvc}
}

func (c *controller) ForManager(managerID string) controllerapi.Controller {
	return &controller{
		svc: c.svc, cfg: c.cfg, cache: c.cache, schedule: c.schedule, managerID: managerID,
	}
}

func (c *controller) BindOutputDelivery(ctx context.Context, data controllerapi.OutputBindingData) error {
	if err := c.requireManagerIdentity(); err != nil {
		return err
	}

	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := outputs.BindManager(ctx, c.managerID, data.Driver, data.Attributes); err != nil {
		return fmt.Errorf("bind output delivery: %w", err)
	}

	if _, err := outputs.RetryBlockedHead(ctx, c.managerID); err != nil {
		return fmt.Errorf("retry blocked output head: %w", err)
	}

	return nil
}

func (c *controller) ClaimOutput(ctx context.Context) (*controllerapi.OutputClaimData, error) {
	outputs := c.outputStore()
	if outputs == nil {
		return nil, errors.New("output delivery is unavailable")
	}

	claim, err := outputs.ClaimOutputHead(ctx, c.managerID)

	var pending *sessionstore.OutputRetryPendingError
	if errors.As(err, &pending) {
		return nil, &controllerapi.OutputRetryPendingError{NextAt: pending.NextAt}
	}

	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil, controllerapi.ErrNoOutput
	}

	if err != nil {
		return nil, fmt.Errorf("claim output head: %w", err)
	}

	data := &controllerapi.OutputClaimData{
		ID:                claim.Output.ID,
		SessionID:         claim.Output.SessionID,
		Type:              string(claim.Output.Type),
		Content:           claim.Output.Content,
		Attributes:        claim.Output.Attributes,
		AttemptID:         claim.Output.AttemptID,
		AttemptSeq:        claim.Output.AttemptSeq,
		SourceKey:         claim.Output.SourceKey,
		SessionAttributes: claim.SessionAttributes,
		ReleasesInput:     claim.Output.ReleasesInput,
	}
	if claim.PreviousDeliveredOutput != nil {
		data.PreviousMessageAttributes = claim.PreviousDeliveredOutput.Attributes
		data.PreviousMessageType = string(claim.PreviousDeliveredOutput.Type)
		data.PreviousModelInputGeneration = generationFromAttributes(claim.PreviousDeliveredOutput.Attributes)
	}

	data.ModelInputGeneration = generationFromAttributes(claim.Output.Attributes)

	return data, nil
}

// generationFromAttributes extracts the host-stamped model-input generation.
// JSON decoding makes stored numbers float64; absence stays nil so legacy rows
// remain distinguishable from a real generation zero.
func generationFromAttributes(attributes map[string]any) *int64 {
	value, ok := attributes[sessionstore.ModelInputGenerationAttribute].(float64)
	if !ok {
		return nil
	}

	generation := int64(value)

	return &generation
}

func (c *controller) CurrentProgress(
	ctx context.Context,
	sessionID int64,
) (*controllerapi.ProgressData, error) {
	if err := c.requireOwnedSession(ctx, sessionID); err != nil {
		return nil, err
	}

	provider, ok := c.svc.(interface {
		CurrentProgress(context.Context, int64) (*controllerapi.ProgressData, error)
	})
	if !ok {
		return nil, errors.New("session progress is unavailable")
	}

	return provider.CurrentProgress(ctx, sessionID)
}

func (c *controller) RefreshProgress(ctx context.Context, sessionID int64) error {
	if err := c.requireOwnedSession(ctx, sessionID); err != nil {
		return err
	}

	provider, ok := c.svc.(interface {
		RefreshProgress(context.Context, int64) error
	})
	if !ok {
		return errors.New("session progress is unavailable")
	}

	return provider.RefreshProgress(ctx, sessionID)
}

func (c *controller) AckOutput(ctx context.Context, data controllerapi.OutputAckData) error {
	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := outputs.AckOutput(
		ctx,
		c.managerID,
		data.ID,
		data.AttemptID,
		data.MessageIDs,
		data.SessionPatch,
	); err != nil {
		return fmt.Errorf("ack output: %w", err)
	}

	if reconciler, ok := c.svc.(interface {
		ReconcileOutputReadiness(context.Context, int64) error
	}); ok {
		if err := reconciler.ReconcileOutputReadiness(ctx, data.ID); err != nil {
			return fmt.Errorf("reconcile output readiness: %w", err)
		}
	}

	return nil
}

func (c *controller) RetryOutput(ctx context.Context, data controllerapi.OutputRetryData) error {
	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := outputs.RetryOutput(ctx, c.managerID, data.ID, data.AttemptID, data.Error, data.NextAt); err != nil {
		return fmt.Errorf("retry output: %w", err)
	}

	return nil
}

func (c *controller) BlockOutput(ctx context.Context, data controllerapi.OutputBlockData) error {
	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if err := outputs.BlockOutput(ctx, c.managerID, data.ID, data.AttemptID, data.Error); err != nil {
		return fmt.Errorf("block output: %w", err)
	}

	return nil
}

func (c *controller) WakeOutput(ctx context.Context) error {
	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	if _, err := outputs.WakeOutputHead(ctx, c.managerID); err != nil {
		return fmt.Errorf("wake output head: %w", err)
	}

	return nil
}

func (c *controller) RepairSessionSurface(ctx context.Context, sessionID int64, binding string) error {
	if err := c.requireOwnedSession(ctx, sessionID); err != nil {
		return err
	}

	outputs := c.outputStore()
	if outputs == nil {
		return errors.New("output delivery is unavailable")
	}

	history, ok := outputs.(sessionstore.LifecycleOutputHistoryStore)
	if !ok {
		return errors.New("output lifecycle history is unavailable")
	}

	record, err := c.svc.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for surface repair: %w", err)
	}

	name, err := c.svc.GetProjectName(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load repair project name: %w", err)
	}

	workDir, err := c.svc.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return fmt.Errorf("load repair work dir: %w", err)
	}

	lifecycleID, err := history.LatestLifecycleOutputID(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load surface repair lifecycle: %w", err)
	}

	digest := sha256.Sum256([]byte(binding))
	bindingHash := hex.EncodeToString(digest[:])
	attributes := map[string]any{"name": name, "work_dir": workDir}

	_, err = outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:  sessionID,
		Type:       sessionstore.OutputSessionOpened,
		Attributes: attributes,
		SourceKey: "session:" + strconv.FormatInt(
			sessionID,
			10,
		) + ":repair:" + strconv.FormatInt(
			lifecycleID,
			10,
		) + ":" + bindingHash,
		Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputSessionOpened, "", sessionID, attributes),
	})
	if err != nil {
		return fmt.Errorf("enqueue session surface repair: %w", err)
	}

	return nil
}

//nolint:funcorder // helper stays beside the delivery capability it supports.
func (c *controller) outputStore() sessionstore.OutputStore {
	source, ok := c.svc.(outputStoreSource)
	if !ok {
		return nil
	}

	return source.OutputStore()
}

func (c *controller) OutputQueueStatus(
	ctx context.Context,
	managerID string,
) (controllerapi.OutputQueueStatusData, error) {
	outputs := c.outputStore()
	if outputs == nil {
		return controllerapi.OutputQueueStatusData{}, errors.New("output delivery is unavailable")
	}

	status, err := outputs.OutputQueueStatus(ctx, managerID)
	if err != nil {
		return controllerapi.OutputQueueStatusData{}, fmt.Errorf("load output queue status: %w", err)
	}

	data := controllerapi.OutputQueueStatusData{
		Pending: status.Pending, BlockedID: status.BlockedID, DeliveryError: status.DeliveryError,
	}
	if status.BlockedAt != nil {
		data.BlockedForSec = int64(time.Since(*status.BlockedAt).Seconds())
	}

	return data, nil
}

func (c *controller) UnresolvedOutputOwners(ctx context.Context) ([]string, error) {
	outputs := c.outputStore()
	if outputs == nil {
		return nil, errors.New("output delivery is unavailable")
	}

	owners, ok := outputs.(sessionstore.OutputOwnerStore)
	if !ok {
		return nil, errors.New("output owner status is unavailable")
	}

	values, err := owners.ListUnresolvedOutputOwners(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unresolved output owners: %w", err)
	}

	return values, nil
}

func (c *controller) CreateSession(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	if err := c.requireManagerIdentity(); err != nil {
		return 0, err
	}

	if data.SystemProject != "" && c.managerID != controllerapi.BuiltinCLIManagerID {
		return 0, errors.New("the reserved system project belongs to the local chat")
	}

	if data.SystemProject != "" && data.SystemProject != controllerapi.CoagentSystemProjectName {
		return 0, fmt.Errorf("unknown system project %q", data.SystemProject)
	}

	if data.SystemProject != "" && data.UseWorktree {
		return 0, fmt.Errorf("system project %q cannot use a worktree", data.SystemProject)
	}

	if data.UseWorktree {
		nextWorkDir, err := createWorktree(ctx, data.WorkDir)
		if err != nil {
			return 0, err
		}

		data.WorkDir = nextWorkDir
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = c.managerID

	projectID, err := c.resolveSessionProject(ctx, data)
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
	_, err := c.SendSessionMessageResolved(ctx, data)
	return err
}

func (c *controller) SendSessionMessageResolved(
	ctx context.Context,
	data controllerapi.SessionMessageData,
) (int64, error) {
	if err := c.requireOwnedSession(ctx, data.SessionID); err != nil {
		return 0, err
	}

	resolver, ok := c.svc.(interface {
		SendToSessionResolved(context.Context, int64, string) (int64, error)
	})
	if !ok {
		if err := c.svc.SendToSession(ctx, data.SessionID, data.Message); err != nil {
			return 0, fmt.Errorf("send session message: %w", err)
		}

		return data.SessionID, nil
	}

	acceptedID, err := resolver.SendToSessionResolved(ctx, data.SessionID, data.Message)
	if err != nil {
		return 0, fmt.Errorf("send resolved session message: %w", err)
	}

	if data.Message != "" {
		c.publishInputReceived(acceptedID, data.Message, "user")
	}

	return acceptedID, nil
}

func (c *controller) ListSessions(ctx context.Context) ([]controllerapi.SessionInfo, error) {
	records, err := c.svc.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	infos := make([]controllerapi.SessionInfo, 0, len(records))

	for _, rec := range records {
		if !c.canListSession(ctx, rec) {
			continue
		}

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

func (c *controller) SetSessionModel(ctx context.Context, data controllerapi.SessionSetModelData) error {
	if err := c.requireOwnedSession(ctx, data.SessionID); err != nil {
		return err
	}

	if err := c.svc.SetModel(ctx, data.SessionID, data.Model, data.ReasoningLevel); err != nil {
		return fmt.Errorf("set session model: %w", err)
	}

	return nil
}

func (c *controller) SetSessionAttributes(ctx context.Context, data controllerapi.SessionSetAttributesData) error {
	if err := c.authorizeAttributeUpdate(ctx, &data); err != nil {
		return err
	}

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
	if err := c.requireOwnedSession(ctx, data.SessionID); err != nil {
		return nil, err
	}

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

func (c *controller) Subscribe() <-chan controllerapi.SessionNotification {
	if c.managerID == "" {
		return make(chan controllerapi.SessionNotification)
	}

	return c.svc.PubSub().SubscribeManager(c.managerID)
}

func (c *controller) Unsubscribe(ch <-chan controllerapi.SessionNotification) {
	c.svc.PubSub().UnsubscribeManager(ch)
}

func (c *controller) resolveSessionProject(ctx context.Context, data controllerapi.SessionCreateData) (int64, error) {
	if data.SystemProject != "" {
		expected := filepath.Join(
			resolveProjectsRoot(c.cfg.UnifiedConfig),
			controllerapi.CoagentSystemProjectDir,
		)
		if !sameProjectPath(data.WorkDir, expected) {
			return 0, errors.New("system project is outside the canonical configuration directory")
		}

		projectID, err := c.svc.GetOrCreateSystemProject(ctx, data.WorkDir, data.SystemProject)
		if err != nil {
			return 0, fmt.Errorf("get system project: %w", err)
		}

		return projectID, nil
	}

	if filepath.Base(filepath.Clean(data.WorkDir)) == controllerapi.CoagentSystemProjectDir {
		return 0, errors.New("reserved system project requires its internal identity")
	}

	projectID, err := c.svc.GetOrCreateProject(ctx, data.WorkDir)
	if err != nil {
		return 0, fmt.Errorf("get project: %w", err)
	}

	return projectID, nil
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
