package localapi

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestStatus_WithoutConfig(t *testing.T) {
	h := newStatusHarness(t, &config.Config{}, true)
	st := h.status(t)

	assert.False(t, st.ConfigPresent)
	assert.Equal(t, statusTestConfigPath, st.ConfigPath)
	assert.Equal(t, statusTestVersion, st.BinaryVersion)
	assert.Equal(t, int64(os.Getpid()), st.Pid)
	assert.Empty(t, st.Providers)
	assert.Empty(t, st.Managers)
	assert.Equal(t, int64(0), st.ModelCount)
}

// A restart reuses the pid, socket and binary, so only the boot id distinguishes runs.
func TestStatus_BootIDNamesTheRunNotTheBinary(t *testing.T) {
	first := newStatusHarness(t, &config.Config{}, true)
	st := first.status(t)
	require.NotEmpty(t, st.BootId)
	again := first.status(t)
	assert.Equal(t, st.BootId, again.BootId, "one run, one id")

	second := newStatusHarness(t, &config.Config{}, true)
	other := second.status(t)
	assert.NotEqual(t, st.BootId, other.BootId)
	assert.Equal(t, st.BinaryVersion, other.BinaryVersion, "same binary — the version cannot tell them apart")
}

func TestStatus_ReportsStaleModels(t *testing.T) {
	for _, stale := range []bool{true, false} {
		t.Run(map[bool]string{true: "stale", false: "fresh"}[stale], func(t *testing.T) {
			cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{Models: []config.ModelEntry{
				{ID: "lost", CatalogStale: stale}, {ID: "fresh"},
			}}}
			st := newStatusHarness(t, cfg, true).status(t)
			if stale {
				assert.Equal(t, []string{"lost"}, st.StaleModels)
			} else {
				assert.Nil(t, st.StaleModels)
			}
		})
	}
}

func TestStatus_WithConfig(t *testing.T) {
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"work":     {Driver: "anthropic"},
			"fallback": {Driver: "openrouter"},
		},
		Models: []config.ModelEntry{
			{ID: "claude-sonnet-5", Provider: "work"},
			{ID: "gpt-5", Provider: "fallback"},
		},
		Managers: []config.ManagerEntry{
			{ID: "tg", Driver: "telegram", Enabled: enabled()},
			{ID: "tg-down", Driver: "telegram", Enabled: enabled()},
		},
	}}

	h := newStatusHarness(t, cfg, true)
	h.managers.running = []string{"tg"}
	h.managers.fail("tg-down", errors.New("get chat: unauthorized"))
	st := h.status(t)

	assert.True(t, st.ConfigPresent)
	assert.Equal(t, int64(2), st.ModelCount)
	assert.Equal(t, "claude-sonnet-5", st.DefaultModel)
	require.Len(t, st.Providers, 2)
	assert.Equal(t, "fallback", st.Providers[0].Name)
	assert.Equal(t, "openrouter", st.Providers[0].Driver)
	assert.Equal(t, "work", st.Providers[1].Name)
	assert.Equal(t, "anthropic", st.Providers[1].Driver)

	require.Len(t, st.Managers, 2)
	assert.Equal(t, "tg", st.Managers[0].Id)
	assert.Equal(t, "telegram", st.Managers[0].Driver)
	assert.True(t, st.Managers[0].Enabled)
	assert.True(t, st.Managers[0].Running)
	assert.Empty(t, st.Managers[0].Error)
	assert.Equal(t, "tg-down", st.Managers[1].Id)
	assert.False(t, st.Managers[1].Running)
	assert.Contains(t, st.Managers[1].Error, "unauthorized")
}

func TestStatus_DisabledManagerCarriesNoError(t *testing.T) {
	off := false
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Managers: []config.ManagerEntry{{ID: "tg", Driver: "telegram", Enabled: &off}},
	}}
	h := newStatusHarness(t, cfg, true)
	h.managers.fail("tg", errors.New("boom"))
	st := h.status(t)

	require.Len(t, st.Managers, 1)
	assert.False(t, st.Managers[0].Enabled)
	assert.Empty(t, st.Managers[0].Error)
}

// A down manager must carry its own failure, never a sibling's.
func TestStatus_ADownManagerCarriesItsOwnReasonOnly(t *testing.T) {
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Managers: []config.ManagerEntry{
			{ID: "tg-one", Driver: "telegram", Enabled: enabled()},
			{ID: "tg-two", Driver: "telegram", Enabled: enabled()},
		},
	}}
	h := newStatusHarness(t, cfg, true)
	h.managers.fail("tg-one", errors.New(`start manager "tg-one": unauthorized`))
	st := h.status(t)

	require.Len(t, st.Managers, 2)
	assert.Contains(t, st.Managers[0].Error, "unauthorized")
	assert.Empty(t, st.Managers[1].Error, "tg-two is down for its own reason, not tg-one's")
}
