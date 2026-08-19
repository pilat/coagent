package managers

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managers/telegram"
)

type Runtime interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	RunningIDs() []string
	StartError(id string) error
}

var _ Runtime = (*runtime)(nil)

type runtime struct {
	cfg         *config.Config
	controllers controllerapi.ManagerControllerFactory
	builder     func(config.ManagerEntry) (Manager, error)

	mu        sync.Mutex
	managers  []Manager
	startErrs map[string]error
}

func NewRuntime(cfg *config.Config, controllers controllerapi.ManagerControllerFactory) Runtime {
	r := &runtime{
		cfg:         cfg,
		controllers: controllers,
		builder:     nil,
	}
	r.builder = r.buildManager

	return r
}

// Start brings up every enabled manager. A manager that refuses to start is
// recorded and skipped rather than fatal: the CLI chat is how a bad bot token
// gets fixed, and it needs the daemon alive to happen.
func (r *runtime) Start(ctx context.Context) error {
	if r.cfg.UnifiedConfig == nil || len(r.cfg.UnifiedConfig.Managers) == 0 {
		return nil
	}

	log := logger.Ctx(ctx).Named("managers.runtime")
	started := make([]Manager, 0, len(r.cfg.UnifiedConfig.Managers))
	failed := make(map[string]error)

	for _, entry := range r.cfg.UnifiedConfig.Managers {
		if entry.Enabled == nil || !*entry.Enabled {
			continue
		}

		mgr, err := r.startOne(ctx, entry)
		if err != nil {
			failed[entry.ID] = err

			log.Error("manager_start_failed", zap.String("manager", entry.ID), zap.Error(err))

			continue
		}

		started = append(started, mgr)
	}

	r.mu.Lock()
	r.managers = started
	r.startErrs = failed
	r.mu.Unlock()

	return nil
}

func (r *runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	list := r.managers
	r.managers = nil
	r.mu.Unlock()

	return r.stopManagers(ctx, list)
}

// RunningIDs lists managers that started, were not stopped, and whose loops are still
// up — `status` diffs it against the configured set to show enabled-but-down.
func (r *runtime) RunningIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.managers))

	for _, m := range r.managers {
		if !m.Alive() {
			continue
		}

		out = append(out, m.ID())
	}

	return out
}

// StartError reports why this manager did not come up, or nil when it started —
// including when it started and died later, which is not a start failure.
func (r *runtime) StartError(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.startErrs[id]
}

func (r *runtime) startOne(ctx context.Context, entry config.ManagerEntry) (Manager, error) {
	if entry.ID == controllerapi.BuiltinCLIManagerID {
		return nil, fmt.Errorf("manager id %q is reserved for the built-in local chat", entry.ID)
	}

	mgr, err := r.builder(entry)
	if err != nil {
		return nil, err
	}

	if err := mgr.Start(ctx); err != nil {
		return nil, fmt.Errorf("start manager %q: %w", mgr.ID(), err)
	}

	return mgr, nil
}

func (r *runtime) stopManagers(ctx context.Context, list []Manager) error {
	var firstErr error

	for _, v := range slices.Backward(list) {
		if err := v.Stop(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop manager %q: %w", v.ID(), err)
		}
	}

	return firstErr
}

func (r *runtime) buildManager(entry config.ManagerEntry) (Manager, error) {
	switch entry.Driver {
	case "telegram":
		mgr, err := telegram.New(entry, r.cfg.UnifiedConfig, r.controllers.ForManager(entry.ID))
		if err != nil {
			return nil, fmt.Errorf("create telegram manager: %w", err)
		}

		return mgr, nil
	default:
		return nil, fmt.Errorf("unknown manager driver %q", entry.Driver)
	}
}
