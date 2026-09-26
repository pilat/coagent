package sandboxpolicy

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_PreservesBuiltinsWithoutOverrides(t *testing.T) {
	testHome(t)

	catalog := testCatalog(t, nil)

	assert.NotEmpty(t, catalog.Version())

	mise, ok := catalog.Profile("mise")
	require.True(t, ok, "the shipped catalog must define mise")
	assert.NotEmpty(t, mise.Mounts)

	assert.Contains(t, catalog.Names(), "git")
	assert.Contains(t, catalog.Names(), "ssh")
	assert.Contains(t, catalog.Names(), "gh")

	// The catalog keeps its declared form; environment names are resolved when a
	// policy is compiled, never while the config is parsed.
	assert.Contains(t, mountsPaths(mise), "${MISE_INSTALLS_DIR}")
	assert.Contains(t, mountsPaths(mise), "~/.local/bin/mise")
}

func TestLoad_DormantProfilesContainOnlyEscalatedEntries(t *testing.T) {
	testHome(t)

	catalog := testCatalog(t, nil)
	names := catalog.Names()
	require.Len(t, names, 53)
	active := map[string]bool{"shell": true, "mise": true, "git": true, "ssh": true, "gh": true}

	for _, name := range names {
		if active[name] {
			continue
		}

		t.Run(name, func(t *testing.T) {
			profile, ok := catalog.Profile(name)
			require.True(t, ok, "the shipped catalog must define %q", name)
			require.NotEmpty(t, profile.Mounts)

			for _, mount := range profile.Mounts {
				assert.Equal(t, LevelEscalated, mount.Type, "%s mount %q must stay dormant", name, mount.Path)
			}
			for _, socket := range profile.Sockets {
				assert.Equal(t, LevelEscalated, socket.Type, "%s socket %q must stay dormant", name, socket.Path)
			}
			for _, network := range profile.Network {
				assert.Equal(t, LevelEscalated, network.Type, "%s network %q must stay dormant", name, network.Address)
			}
		})
	}
}

func TestLoad_OverrideReplacesBuiltinInFull(t *testing.T) {
	home := testHome(t)
	replacement := Profile{Mounts: []Mount{{
		Path: filepath.Join(home, "git-data"), Mode: ModeReadOnly, Type: LevelBasic,
	}}}

	catalog := testCatalog(t, map[string]Profile{"git": replacement})

	git, ok := catalog.Profile("git")
	require.True(t, ok)
	assert.Equal(t, replacement.Mounts, git.Mounts, "an override replaces the built-in definition in full")
	assert.Empty(t, git.Sockets)
}

func TestLoad_AddsOperatorProfile(t *testing.T) {
	home := testHome(t)
	custom := Profile{Mounts: []Mount{{
		Path: filepath.Join(home, "custom-data"), Mode: ModeReadWrite, Type: LevelEscalated,
	}}}

	catalog := testCatalog(t, map[string]Profile{"custom": custom})

	profile, ok := catalog.Profile("custom")
	require.True(t, ok)
	assert.Equal(t, custom.Mounts, profile.Mounts)
	assert.Contains(t, catalog.Names(), "custom")
}

func TestLoad_RejectsInvalidOverride(t *testing.T) {
	home := testHome(t)

	_, err := Load(map[string]Profile{"broken": {Mounts: []Mount{{
		Path: filepath.Join(home, "data"), Mode: "wo", Type: LevelBasic,
	}}}})

	assert.ErrorContains(t, err, "invalid mode")
}

func TestCatalogEnvironment_DerivesToolDirectoriesFromOverrides(t *testing.T) {
	values := map[string]string{
		"XDG_CONFIG_HOME": "/operator/config",
		"XDG_DATA_HOME":   "/operator/data",
		"MISE_CACHE_DIR":  "/operator/mise-cache",
	}
	for name, want := range map[string]string{
		"GH_CONFIG_DIR":     "/operator/config/gh",
		"MISE_CONFIG_DIR":   "/operator/config/mise",
		"MISE_DATA_DIR":     "/operator/data/mise",
		"MISE_INSTALLS_DIR": "/operator/data/mise/installs",
		"MISE_CACHE_DIR":    "/operator/mise-cache",
	} {
		got, ok := catalogEnvValue(name, values)
		require.True(t, ok, name)
		assert.Equal(t, want, got, name)
	}
	values["MISE_DATA_DIR"] = "/operator/shared-mise"
	values["MISE_INSTALLS_DIR"] = "/operator/installed"
	got, ok := catalogEnvValue("MISE_INSTALLS_DIR", values)
	require.True(t, ok)
	assert.Equal(t, "/operator/installed", got)
}

func mountsPaths(profile Profile) []string {
	paths := make([]string, 0, len(profile.Mounts))

	for _, mount := range profile.Mounts {
		paths = append(paths, mount.Path)
	}

	return paths
}
