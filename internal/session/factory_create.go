package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

// Create produces an isolated session for the given workdir.
func (f *factory) Create(ctx context.Context, opts CreateOptions) (Service, error) {
	if opts.ID == 0 {
		return nil, errors.New("session ID is required")
	}

	if opts.WorkDir == "" {
		return nil, errors.New("workdir is required")
	}

	if opts.OutputEnabled && f.outputStore == nil {
		return nil, errors.New("output store is required when output is enabled")
	}

	ms := newMessageStore(f.store, opts.ID, f.outputStore)
	if f.store != nil {
		if err := ms.reloadMessages(ctx); err != nil {
			return nil, fmt.Errorf("load messages: %w", err)
		}
	}

	if opts.TranscriptOnly {
		return &svc{ms: ms, stagedCalls: opts.StagedExternalCalls}, nil
	}

	cfg := f.sessionConfig(opts.WorkDir, opts.Model, opts.RepoRoot)

	llmClient, err := f.newLLMClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create LLM client: %w", err)
	}

	// Ownership moves to Service only after every session dependency is built;
	// earlier failures must close the client here.
	owned := true
	defer func() {
		if owned {
			_ = llmClient.Close()
		}
	}()

	sess, err := f.build(ctx, cfg, llmClient, opts, ms.getMessages(), ms.getRowIDs())
	if err != nil {
		return nil, err
	}

	owned = false

	return sess, nil
}

func (f *factory) sessionConfig(workDir, model, repoRoot string) *config.Config {
	cfg := *f.cfg
	cfg.WorkDir = workDir
	cfg.RepoRoot = repoRoot

	if model != "" {
		cfg.Model = model
	}

	if cfg.Model == "" {
		cfg.Model = cfg.DefaultModel()
	}

	return &cfg
}

//nolint:funlen // Session construction preserves cleanup adjacency across stack ownership transfer.
func (f *factory) build(
	ctx context.Context,
	cfg *config.Config,
	llmClient llm.Client,
	opts CreateOptions,
	messages []llmwire.Message,
	rowIDs []int64,
) (Service, error) {
	var todoItems []*todo.Item

	if opts.TodoItems != "" && opts.TodoItems != "[]" {
		if err := json.Unmarshal([]byte(opts.TodoItems), &todoItems); err != nil {
			return nil, fmt.Errorf("unmarshal todo items: %w", err)
		}
	}

	todoSvc := todo.New()
	ldr := loader.New(f.marketplaceCache)

	reg, stack, err := f.buildRegistry(
		ctx, cfg, ldr, todoSvc, opts.ProjectID, opts.ID, opts.RootID, opts.ShieldsUp, opts.CreatedWorktree,
	)
	if err != nil {
		return nil, err
	}

	var resumeMessages []llmwire.Message
	var resumeRowIDs []int64

	if len(messages) > 0 {
		resumeMessages = messages
		resumeRowIDs = rowIDs
	}

	p := params{
		Config:       cfg,
		LLMClient:    llmClient,
		TodoStore:    todoSvc,
		Loader:       ldr,
		Stack:        stack,
		Registry:     reg,
		Store:        f.store,
		OutputStore:  f.outputStore,
		Dispositions: dispositionsStore(f.store),
		GitClient:    f.gitClient,
		MemoryStore:  f.memoryStore,
	}

	sessOpts := options{
		ID:                    opts.ID,
		AgentType:             registry.AgentType(opts.AgentType),
		ProjectID:             opts.ProjectID,
		RootID:                opts.RootID,
		ReasoningLevel:        opts.ReasoningLevel,
		ResumeMessages:        resumeMessages,
		ResumeRowIDs:          resumeRowIDs,
		ResumeIteration:       opts.Iteration,
		ResumeTodoItems:       todoItems,
		LastActivityAt:        opts.LastActivityAt,
		InputBoundary:         opts.InputBoundary,
		OutputEnabled:         opts.OutputEnabled,
		BudgetGate:            opts.BudgetGate,
		SettlementOpen:        opts.SettlementOpen,
		PreserveStopped:       opts.PreserveStoppedStatus,
		ActiveSubagents:       opts.ActiveSubagents,
		ActiveProcesses:       opts.ActiveProcesses,
		ContextBaseline:       opts.ContextBaseline,
		ResumeCompletionState: opts.ResumeCompletionState,

		ActiveSubagentsProvider:  opts.ActiveSubagentsProvider,
		ActiveProcessesProvider:  opts.ActiveProcessesProvider,
		OnIterationPersisted:     opts.OnIterationPersisted,
		ExtraSkills:              opts.ExtraSkills,
		StagedExternalCalls:      opts.StagedExternalCalls,
		CompactionDeferAnnounced: opts.CompactionDeferAnnounced,
	}

	sess, err := newWithOptions(ctx, p, sessOpts)
	if err != nil {
		if stack != nil {
			_ = stack.Close()
		}

		return nil, err
	}

	return sess, nil
}

// dispositionsStore projects the response-disposition capability off the
// runtime store; nil store keeps the loop on its in-memory test paths.
func dispositionsStore(store sessionstore.RuntimeStore) sessionstore.ResponseDispositionStore {
	if store == nil {
		return nil
	}

	if d, ok := store.(sessionstore.ResponseDispositionStore); ok {
		return d
	}

	return nil
}
