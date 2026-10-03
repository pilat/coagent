package sessionbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// BuildInput combines construction dependencies with one activation snapshot.
type BuildInput struct {
	Record                   *sessionstore.SessionRecord
	Config                   *config.Config
	Secrets                  config.Secrets
	MemoryStore              memory.CuratedStore
	Store                    session.Store
	GitClient                git.Client
	MCPStore                 mcpstore.Store
	MarketplaceCache         loader.MarketplaceCache
	ProcessService           backgroundprocess.Service
	Resources                builtin.Resources
	Loader                   loader.Service
	OwnerTools               []tool.Tool
	ExternalCalls            map[string]string
	Events                   session.Events
	WorkDir                  string
	RepoRoot                 string
	ExtraSkills              []*loader.Skill
	Schedules                string
	Status                   string
	OutputEnabled            bool
	PreserveStoppedStatus    bool
	ActiveSubagents          []sessionprompt.ActiveSubagentInfo
	ActiveProcesses          []sessionprompt.ActiveProcessInfo
	CompactionDeferAnnounced bool
}

// Build transfers the model to the session and returns its tool-stack release.
func Build(ctx context.Context, in BuildInput) (*session.Session, func(), error) {
	if in.Record == nil || in.Record.ID == 0 || in.WorkDir == "" || in.Store == nil || in.Config == nil {
		return nil, nil, errors.New("session record, workdir and store are required")
	}

	cfg := *in.Config
	cfg.WorkDir, cfg.RepoRoot = in.WorkDir, in.RepoRoot

	cfg.Model = in.Record.Model
	if cfg.Model == "" {
		cfg.Model = cfg.DefaultModel()
	}

	in.Config = &cfg
	if in.Loader == nil {
		in.Loader = loader.New(in.MarketplaceCache)
	}

	client, err := llm.NewClient(&cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create LLM client: %w", err)
	}

	sess, cleanup, err := build(ctx, in, client)
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}

	return sess, cleanup, nil
}

// RetireToolResources releases cached tools after a stopped session joins.
func RetireToolResources(resources builtin.Resources, sessionID int64) error {
	if resources == nil {
		return nil
	}

	if err := resources.Retire(sessionID); err != nil {
		return fmt.Errorf("retire session tool resources: %w", err)
	}

	return nil
}

// CloseToolResources releases the shared cache after all runners join.
func CloseToolResources(resources builtin.Resources) error {
	if resources == nil {
		return nil
	}

	if err := resources.Close(); err != nil {
		return fmt.Errorf("close session tool resources: %w", err)
	}

	return nil
}

func build(ctx context.Context, in BuildInput, client llm.Client) (*session.Session, func(), error) {
	set := registry.NewSet(loadProjectSubagents(ctx, in, in.WorkDir))

	agentType := registry.AgentType(in.Record.AgentType)
	if agentType == "" {
		agentType = registry.AgentTypeBuild
	}

	agentConfig, ok := set.Get(agentType)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent type: %s", agentType)
	}

	openingContext, skills := loadOpeningContext(ctx, in, agentConfig)

	todos, err := restoreTodos(in.Record.TodoItems)
	if err != nil {
		return nil, nil, err
	}

	reg, stack, err := buildRegistry(ctx, in, todos)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { _ = stack.Close() }) }

	attachImageAuthorizer(client, stack)
	configureClient(client, in.Record)
	registerSessionTools(reg, in)

	reg = filterRegistryForAgent(set, reg, agentConfig)
	if reg.Get("batch") != nil {
		reg.Register(builtin.NewBatchTool(reg))
	}

	prompt := preparePrompt(in, agentConfig, skills, todos, reg, stack)

	sess, err := session.New(ctx, session.Input{
		Record:                   in.Record,
		Client:                   client,
		Loader:                   in.Loader,
		Registry:                 reg,
		Prompt:                   prompt,
		Store:                    in.Store,
		Events:                   in.Events,
		ExternalCalls:            in.ExternalCalls,
		OpeningContext:           openingContext,
		OutputEnabled:            in.OutputEnabled,
		PreserveStopped:          in.PreserveStoppedStatus,
		Schedules:                in.Schedules,
		Status:                   in.Status,
		BackgroundSnapshot:       sessionprompt.BuildActiveBackgroundSection(in.ActiveProcesses, in.ActiveSubagents),
		CompactionDeferAnnounced: in.CompactionDeferAnnounced,
		ImageAuthorizer:          stack.Access(),
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("build: %w", err)
	}

	return sess, cleanup, nil
}

func loadOpeningContext(ctx context.Context, in BuildInput, cfg registry.AgentTypeConfig) (string, []*loader.Skill) {
	if cfg.OmitProjectContext {
		return "", nil
	}

	openingContext := loadProjectInstructions(ctx, in, in.WorkDir)
	if in.MemoryStore != nil && in.Record.ProjectID != 0 {
		openingContext += sessionprompt.BuildMemoriesSection(ctx, in.MemoryStore, in.Record.ProjectID)
	}

	for _, skill := range in.ExtraSkills {
		in.Loader.RegisterSkill(skill)
	}

	return openingContext, in.ExtraSkills
}

func restoreTodos(raw string) (todo.Service, error) {
	todos := todo.New()
	if raw == "" {
		return todos, nil
	}

	var items []*todo.Item
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("restore todo items: %w", err)
	}

	todos.Replace(items)

	return todos, nil
}

func configureClient(client llm.Client, record *sessionstore.SessionRecord) {
	client.SetReasoningLevel(string(llm.ReasoningMedium))

	if record.ReasoningLevel != "" {
		client.SetReasoningLevel(record.ReasoningLevel)
	}

	sessionID := strconv.FormatInt(record.ID, 10)
	if record.RootID != 0 && record.RootID != record.ID {
		sessionID = fmt.Sprintf("%d:%d", record.RootID, record.ID)
	}

	client.SetSessionID(sessionID)
}

func preparePrompt(
	in BuildInput,
	cfg registry.AgentTypeConfig,
	skills []*loader.Skill,
	todos todo.Service,
	reg tool.Registry,
	stack *builtin.Stack,
) *sessionprompt.Builder {
	prompt := buildPrompt(in, cfg, skills)

	prompt.Todos, prompt.WorkDir = todos, in.WorkDir
	if !cfg.OmitProjectContext {
		prompt.GitClient = confineGitClient(in.GitClient, stack)
	}

	prompt.RefreshToolsSection(reg)

	if reg.Get(tool.IDSkill) != nil {
		prompt.SetSkillsSection(sessionprompt.BuildSkillsSection(in.Loader))
	}

	if reg.Get(tool.IDTask) != nil {
		prompt.SetSubagentsSection(sessionprompt.BuildSubagentsSection(in.Loader))
	}

	return prompt
}

func buildRegistry(ctx context.Context, in BuildInput, todos todo.Service) (tool.Registry, *builtin.Stack, error) {
	rootID := in.Record.RootID
	if rootID == 0 {
		rootID = in.Record.ID
	}

	stack, err := builtin.BuildStack(ctx, builtin.StackConfig{
		ProjectID:       in.Record.ProjectID,
		SessionID:       in.Record.ID,
		RootSessionID:   rootID,
		WorkDir:         in.WorkDir,
		RepoRoot:        in.RepoRoot,
		Servers:         resolveMCPServers(ctx, in.MCPStore, in.Secrets, in.Record.ProjectID),
		Unified:         in.Config.UnifiedConfig,
		Loader:          in.Loader,
		Todo:            todos,
		TodoReplacement: &todoReplacement{memory: todos},
		FileReadTracker: &fileReadTracker{
			store:     in.Store,
			sessionID: in.Record.ID,
		},
		ProcessService: in.ProcessService,
		Resources:      in.Resources,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build tool stack: %w", err)
	}

	return stack.Registry, stack, nil
}

func registerSessionTools(reg tool.Registry, in BuildInput) {
	reg.Register(builtin.NewSkillTool(in.Loader))

	if in.Record.ProjectID != 0 && in.MemoryStore != nil {
		reg.Register(builtin.NewMemorySaveTool(in.MemoryStore, in.Record.ProjectID))
		reg.Register(builtin.NewMemoryDeleteTool(in.MemoryStore, in.Record.ProjectID))
	}

	for _, ownerTool := range in.OwnerTools {
		reg.Register(ownerTool)
	}
}

func filterRegistryForAgent(set *registry.Set, reg tool.Registry, cfg registry.AgentTypeConfig) tool.Registry {
	return reg.Filter(set.FilterTools(reg.IDs(), cfg.Name))
}

func buildPrompt(in BuildInput, cfg registry.AgentTypeConfig, skills []*loader.Skill) *sessionprompt.Builder {
	base := cfg.Prompt + fmt.Sprintf(
		"\n\n# Environment\n- Working directory: %s\n- Platform: %s/%s\n- Timestamped user input is prefixed with `[+elapsed DOW YYYY-MM-DD HH:MM ZONE ±HH:MM]`, where `+elapsed` is optional. Use it for temporal reasoning.",
		in.WorkDir,
		runtime.GOOS,
		runtime.GOARCH,
	)

	models := ""
	if !cfg.OmitProjectContext {
		models = sessionprompt.BuildModelsSection(in.Config.Model)
	}

	prompt := sessionprompt.NewBuilder(base, models, skills...)
	prompt.SetNativeSearch(in.Config.UnifiedConfig.SearchNativeActive(in.Config.Model))

	return prompt
}

func attachImageAuthorizer(client llm.Client, stack *builtin.Stack) {
	client.SetImageAuthorizer(stack.Access())
}

func confineGitClient(client git.Client, stack *builtin.Stack) git.Client {
	if client != nil && stack.SandboxedGit() != nil {
		return stack.SandboxedGit()
	}

	return client
}
