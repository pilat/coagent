package localapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/config"
)

// ManagerRuntime supplies each manager's liveness and own startup failure.
type ManagerRuntime interface {
	RunningIDs() []string
	StartError(id string) error
}

// StatusSource supplies daemon status. Config may carry a nil UnifiedConfig —
// that is the legal pre-configuration state, not an error.
type StatusSource struct {
	Config     *config.Config
	ConfigPath string
	Version    string
	Managers   ManagerRuntime
}

type daemonService struct {
	source  StatusSource
	bootID  string
	started time.Time
	ready   *atomic.Bool
}

func (s *daemonService) GetStatus(context.Context, *coagentv1.GetStatusRequest) (*coagentv1.GetStatusResponse, error) {
	if !s.ready.Load() {
		return nil, connect.Errorf(connect.CodeUnavailable, "daemon is starting")
	}

	return s.status(), nil
}

func (s *daemonService) status() *coagentv1.GetStatusResponse {
	cfg := s.source.Config
	out := &coagentv1.GetStatusResponse{
		BinaryVersion: s.source.Version,
		BootId:        s.bootID,
		Pid:           int64(os.Getpid()),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		ConfigPath:    s.source.ConfigPath,
		ConfigPresent: cfg != nil && cfg.UnifiedConfig != nil,
	}

	if out.ConfigPresent {
		out.Providers = providerStatuses(cfg.UnifiedConfig.Providers)
		out.ModelCount = int64(len(cfg.UnifiedConfig.Models))
		out.StaleModels = staleModels(cfg.UnifiedConfig.Models)
		out.DefaultModel = cfg.DefaultModel()
		out.Search = searchStatus(cfg.UnifiedConfig, cfg.DefaultModel())
		out.Managers = s.managerStatuses(cfg.UnifiedConfig.Managers)
	}

	return out
}

func (s *daemonService) managerStatuses(entries []config.ManagerEntry) []*coagentv1.ManagerStatus {
	running := map[string]struct{}{}

	if s.source.Managers != nil {
		for _, id := range s.source.Managers.RunningIDs() {
			running[id] = struct{}{}
		}
	}

	out := make([]*coagentv1.ManagerStatus, 0, len(entries))
	for _, m := range entries {
		_, isRunning := running[m.ID]

		st := &coagentv1.ManagerStatus{
			Id:      m.ID,
			Driver:  m.Driver,
			Enabled: m.Enabled != nil && *m.Enabled,
			Running: isRunning,
		}
		if st.Enabled && !isRunning {
			st.Error = s.managerError(m.ID)
		}

		out = append(out, st)
	}

	return out
}

// managerError is why this manager is down — its own reason, never a sibling's.
func (s *daemonService) managerError(id string) string {
	if s.source.Managers == nil {
		return ""
	}

	err := s.source.Managers.StartError(id)
	if err == nil {
		return ""
	}

	return err.Error()
}

func providerStatuses(providers map[string]config.ProviderEntry) []*coagentv1.ProviderStatus {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}

	slices.Sort(names)

	out := make([]*coagentv1.ProviderStatus, 0, len(names))
	for _, name := range names {
		out = append(out, &coagentv1.ProviderStatus{Name: name, Driver: providers[name].Driver})
	}

	return out
}

func staleModels(models []config.ModelEntry) []string {
	var out []string

	for _, model := range models {
		if model.CatalogStale {
			out = append(out, model.ID)
		}
	}

	return out
}

// searchStatus mirrors session precedence: an explicit REST provider beats
// native passthrough, and an explicit disable beats everything.
func searchStatus(unified *config.UnifiedConfig, defaultModel string) string {
	search := unified.Tools.Search
	switch {
	case search.SearchDisabled():
		return "disabled"
	case search.Provider == config.SearchProviderTavily:
		return "tavily"
	case search.Provider == config.SearchProviderSearxng:
		return "searxng (" + search.BaseURL + ")"
	}

	if unified.SearchNativeActive(defaultModel) {
		return "native (openrouter)"
	}

	return ""
}

// newBootID names this run. Clients compare it for equality; a clock is not an identity.
func newBootID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}
