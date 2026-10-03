package daemon

import (
	"cmp"
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type models struct {
	defaultModel string
	entries      []config.ModelEntry
	infos        []subagent.ModelInfo
}

func newModels(cfg *config.Config) models {
	result := models{defaultModel: cfg.DefaultModel()}
	if cfg.UnifiedConfig != nil {
		result.entries = cfg.UnifiedConfig.Models
	}

	for _, entry := range result.entries {
		result.infos = append(result.infos, subagent.ModelInfo{ID: entry.ID, Name: entry.Name, Tags: entry.Tags})
	}

	return result
}

// The marker crosses JSON as a bool or string; only explicit false-like values opt out.
func isManagementSurface(attrs map[string]any) bool {
	v, ok := attrs[controllerapi.SessionAttributeManagementSurface]
	if !ok || v == nil {
		return false
	}

	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false" && v != "0"
	default:
		return true
	}
}

func (s *svc) openSession(
	ctx context.Context,
	sessionID int64,
	workDir string,
	rec *sessionstore.SessionRecord,
	preserveStopped bool,
) (*session.Session, func(), error) {
	externalCalls, err := s.callOwners(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	repoRoot, err := s.sessionRepoRoot(ctx, rec)
	if err != nil {
		return nil, nil, err
	}

	in := s.build
	in.Record, in.WorkDir, in.RepoRoot = rec, workDir, repoRoot
	in.ExternalCalls = externalCalls
	in.Events = &sessionEvents{daemon: s, sessionID: sessionID}
	in.CompactionDeferAnnounced = s.runners.deferAnnounced(sessionID)
	in.PreserveStoppedStatus = preserveStopped
	in.Loader = loader.New(in.MarketplaceCache)

	schedules, schedulesErr := s.schedules.Render(ctx, sessionID)
	if schedulesErr != nil {
		return nil, nil, fmt.Errorf("capture construction schedules: %w", schedulesErr)
	}

	in.Schedules = schedules

	status, statusErr := s.progress.Current(ctx, sessionRootID(rec))
	if statusErr != nil {
		return nil, nil, fmt.Errorf("capture construction status: %w", statusErr)
	}

	in.Status = status.Rendered

	owner, _ := rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
	in.OutputEnabled = rec.ParentID == 0 && owner != ""
	in.ActiveSubagents = s.activeSubagentInfos(ctx, sessionID)
	in.ActiveProcesses = s.activeProcessInfos(ctx, sessionID)

	if rec.ParentID == 0 && isManagementSurface(rec.Attributes) {
		skill, err := loader.BuiltinSkill(loader.ManagementSkillName)
		if err != nil {
			return nil, nil, fmt.Errorf("load management skill: %w", err)
		}

		in.ExtraSkills = append(in.ExtraSkills, skill)
	}

	in.OwnerTools = s.ownerTools(rec, in.Loader)

	sess, cleanup, err := sessionbuild.Build(ctx, in)
	if err != nil {
		return nil, nil, fmt.Errorf("assemble session: %w", err)
	}

	return sess, cleanup, nil
}

// Children do not copy manager-owned attributes, so confinement follows the durable root.
func (s *svc) sessionRepoRoot(ctx context.Context, rec *sessionstore.SessionRecord) (string, error) {
	if rec.RootID == 0 {
		repoRoot, _ := rec.Attributes["repo_root"].(string)

		return repoRoot, nil
	}

	root, err := s.store.GetSession(ctx, rec.RootID)
	if err != nil {
		return "", fmt.Errorf("load root session %d for worktree policy: %w", rec.RootID, err)
	}

	if root == nil || root.ProjectID != rec.ProjectID {
		return "", fmt.Errorf("root session %d does not match project %d", rec.RootID, rec.ProjectID)
	}

	repoRoot, _ := root.Attributes["repo_root"].(string)

	return repoRoot, nil
}

// Ownership comes from producer ledgers and exact queued results, never tool names alone.
func (s *svc) callOwners(ctx context.Context, sessionID int64) (map[string]string, error) {
	stored, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	unresolved, err := session.UnresolvedStoredCalls(stored)
	if err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	actual := make(map[string]string, len(unresolved))
	for _, call := range unresolved {
		actual[call.ID] = call.Name
	}

	calls := s.applier.Calls(sessionID)

	if calls == nil {
		calls = make(map[string]string)
	}

	owed, err := s.applier.PendingCall(sessionID)

	switch {
	case err != nil:
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("read_pending_apply_marker", zap.Int64("session_id", sessionID), zap.Error(err))
	case owed.ToolCallID != "":
		calls[owed.ToolCallID] = owed.ToolName
	}

	sleeps, err := s.schedules.PendingSleeps(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending sleeps for session %d: %w", sessionID, err)
	}

	for _, sleep := range sleeps {
		calls[sleep.CallID] = tool.IDSleep
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending child calls for session %d: %w", sessionID, err)
	}

	for _, link := range links {
		if link.Blocking && link.TaskCallID != "" {
			calls[link.TaskCallID] = tool.IDTask
		}
	}

	// Producer delivery atomically trades its ledger claim for a queued result.
	if err := s.pendingResultOwners(ctx, sessionID, actual, calls); err != nil {
		return nil, fmt.Errorf("load pending external calls: %w", err)
	}

	for callID, name := range calls {
		if actual[callID] != name {
			delete(calls, callID)
		}
	}

	return calls, nil
}

func (s *svc) activeSubagentInfos(ctx context.Context, sessionID int64) []sessionprompt.ActiveSubagentInfo {
	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_pending_child_links", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	if len(links) == 0 {
		return nil
	}

	infos := make([]sessionprompt.ActiveSubagentInfo, 0, len(links))
	for _, l := range links {
		infos = append(infos, sessionprompt.ActiveSubagentInfo{
			ChildID:  l.ChildID,
			Blocking: l.Blocking,
			State:    string(l.State),
		})
	}

	return infos
}

func (s *svc) activeProcessInfos(ctx context.Context, sessionID int64) []sessionprompt.ActiveProcessInfo {
	processes, err := s.processStore.ListRunningBySessions(ctx, []int64{sessionID})
	if err != nil {
		logger.Ctx(ctx).Named("daemon.runner").
			Warn("list_running_processes", zap.Int64("session_id", sessionID), zap.Error(err))

		return nil
	}

	infos := make([]sessionprompt.ActiveProcessInfo, 0, len(processes))
	for _, process := range processes {
		if process.AdvertisedAt == nil {
			continue
		}

		infos = append(infos, sessionprompt.ActiveProcessInfo{ID: process.ID, OutputPath: process.OutputPath})
	}

	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })

	return infos
}

func (s *svc) ownerTools(rec *sessionstore.SessionRecord, ldr loader.Service) []tool.Tool {
	tools := []tool.Tool{
		subagent.NewTaskTool(subagent.Spawner(s), rec.ID, ldr, s.models.infos),
		subagent.NewGetSubagentResultTool(subagent.Spawner(s)), subagent.NewSendToSubagentTool(subagent.Spawner(s)),
	}

	if rec.ParentID == 0 {
		tools = append(tools, schedule.NewScheduleTool(rec.ID, s.schedules, time.Local))
	}

	tools = append(tools, s.schedules.SleepTool(rec.ID))

	if rec.ParentID == 0 {
		tools = append(tools, mcpstore.NewTools(s.mcpStore, rec.ProjectID)...)

		tools = append(tools,
			configapply.NewConfigEdit(rec.ID, s.applier),
			budget.NewTool(budget.Store(s.store), rec.ID, s.models.priced(rec.Model)),
		)
	}

	return tools
}

func (s *svc) pendingResultOwners(ctx context.Context, sessionID int64, actual, calls map[string]string) error {
	pending, err := s.store.ListPending(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load pending result owners: %w", err)
	}

	for _, row := range pending {
		if row.Source == sessionstore.InputSourceCallResult {
			callID, _ := row.Attributes["call_id"].(string)

			toolID, _ := row.Attributes["tool_id"].(string)
			if actual[callID] == toolID && toolID != "" {
				calls[callID] = toolID
			}
		}
	}

	return nil
}

func (m models) configured(id string) error {
	if len(m.infos) == 0 {
		return nil
	}

	for _, info := range m.infos {
		if info.ID == id {
			return nil
		}
	}

	return fmt.Errorf("unknown model: %s", id)
}

func (m models) priced(id string) bool {
	for _, model := range m.entries {
		if model.ID == id {
			return model.Pricing != nil
		}
	}

	return false
}

// Explicit effort must fit the child's model; inherited effort can fall back to its default.
func (m models) effort(model, requested, inherited string) (string, error) {
	if len(m.entries) == 0 {
		return cmp.Or(requested, inherited), nil
	}

	if requested != "" {
		level, err := sessionbuild.ResolveReasoningLevel(m.entries, model, requested)
		if err != nil {
			return "", fmt.Errorf("spawn subagent on model %s: %w", model, err)
		}

		return level, nil
	}

	if level, err := sessionbuild.ResolveReasoningLevel(m.entries, model, inherited); err == nil {
		return level, nil
	}

	level, err := sessionbuild.ResolveReasoningLevel(m.entries, model, "")
	if err != nil {
		return "", fmt.Errorf("spawn subagent on model %s: %w", model, err)
	}

	return level, nil
}
