package managers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

type fakeManager struct {
	id       string
	startErr error
	dead     bool
	started  int
	stopped  int
}

func (m *fakeManager) ID() string {
	return m.id
}

func (m *fakeManager) Start(context.Context) error {
	m.started++
	return m.startErr
}

func (m *fakeManager) Stop(context.Context) error {
	m.stopped++
	return nil
}

func (m *fakeManager) Alive() bool {
	return !m.dead
}

// runtimeFor wires one enabled config entry per fake, so a test only has to
// describe the managers it cares about.
func runtimeFor(mgrs ...*fakeManager) *runtime {
	enabled := true
	entries := make([]config.ManagerEntry, 0, len(mgrs))
	byID := make(map[string]Manager, len(mgrs))

	for _, m := range mgrs {
		entries = append(entries, config.ManagerEntry{ID: m.id, Driver: "telegram", Enabled: &enabled})
		byID[m.id] = m
	}

	return &runtime{
		cfg:     &config.Config{UnifiedConfig: &config.UnifiedConfig{Managers: entries}},
		builder: func(entry config.ManagerEntry) (Manager, error) { return byID[entry.ID], nil },
	}
}

func TestRuntime_OnStart_StartsOnlyEnabledManagers(t *testing.T) {
	enabled := true
	disabled := false

	cfg := &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Managers: []config.ManagerEntry{
				{ID: "enabled", Driver: "telegram", Enabled: &enabled},
				{ID: "disabled", Driver: "telegram", Enabled: &disabled},
			},
		},
	}

	enabledMgr := &fakeManager{id: "enabled"}
	r := &runtime{
		cfg: cfg,
		builder: func(entry config.ManagerEntry) (Manager, error) {
			if entry.ID == "enabled" {
				return enabledMgr, nil
			}
			return &fakeManager{id: entry.ID}, nil
		},
	}

	require.NoError(t, r.Start(context.Background()))
	assert.Equal(t, []string{"enabled"}, r.RunningIDs())
	assert.Equal(t, 1, enabledMgr.started)

	require.NoError(t, r.Stop(context.Background()))
	assert.Equal(t, 1, enabledMgr.stopped)
}

func TestRuntime_RejectsBuiltinCLIManagerID(t *testing.T) {
	reserved := &fakeManager{id: "cli"}
	r := runtimeFor(reserved)

	require.NoError(t, r.Start(context.Background()))
	assert.Empty(t, r.RunningIDs())
	assert.Equal(t, 0, reserved.started)
	require.ErrorContains(t, r.StartError("cli"), "reserved for the built-in local chat")
}

func TestRuntime_OnStart_SkipsFailedManagerAndKeepsTheRest(t *testing.T) {
	first := &fakeManager{id: "first"}
	second := &fakeManager{id: "second", startErr: errors.New("boom")}
	third := &fakeManager{id: "third"}

	r := runtimeFor(first, second, third)

	require.NoError(t, r.Start(context.Background()))
	assert.Equal(t, 1, first.started)
	assert.Equal(t, 1, second.started)
	assert.Equal(t, 1, third.started)
	assert.Equal(t, []string{"first", "third"}, r.RunningIDs())

	// Each manager answers for itself: the healthy ones carry no reason at all,
	// and the failed one carries its own.
	require.NoError(t, r.StartError("first"))
	require.NoError(t, r.StartError("third"))
	require.Error(t, r.StartError("second"))
	assert.Contains(t, r.StartError("second").Error(), `start manager "second"`)
	assert.Contains(t, r.StartError("second").Error(), "boom")

	require.NoError(t, r.Stop(context.Background()))
	assert.Equal(t, 1, first.stopped)
	assert.Equal(t, 0, second.stopped)
	assert.Equal(t, 1, third.stopped)
	assert.Empty(t, r.RunningIDs())
}

func TestRuntime_OnStart_ReportsNoErrorWhenEveryManagerStarts(t *testing.T) {
	enabled := true

	cfg := &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Managers: []config.ManagerEntry{{ID: "only", Driver: "telegram", Enabled: &enabled}},
		},
	}

	r := &runtime{
		cfg:     cfg,
		builder: func(entry config.ManagerEntry) (Manager, error) { return &fakeManager{id: entry.ID}, nil },
	}

	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.StartError("only"))
	assert.Equal(t, []string{"only"}, r.RunningIDs())
}

// "Running" must mean the manager's own loops are still up, not that Start
// returned nil once: a bot whose poller died reads as running forever otherwise.
func TestRuntime_RunningIDs_DropsAManagerWhoseLoopsDied(t *testing.T) {
	on := true

	cfg := &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Managers: []config.ManagerEntry{
				{ID: "alive", Driver: "telegram", Enabled: &on},
				{ID: "died", Driver: "telegram", Enabled: &on},
			},
		},
	}

	died := &fakeManager{id: "died"}

	r := &runtime{
		cfg: cfg,
		builder: func(entry config.ManagerEntry) (Manager, error) {
			if entry.ID == "died" {
				return died, nil
			}

			return &fakeManager{id: entry.ID}, nil
		},
	}

	require.NoError(t, r.Start(context.Background()))
	assert.Equal(t, []string{"alive", "died"}, r.RunningIDs())

	died.dead = true

	assert.Equal(t, []string{"alive"}, r.RunningIDs())
	require.NoError(t, r.StartError("died"), "it started fine — it died afterwards")
}
