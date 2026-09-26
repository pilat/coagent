package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func sandboxTestHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Cleanup(coagenthome.Override(home))

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "agent.sock"))

	return home
}

func TestValidateSandbox_AcceptsProfilesAndEscalation(t *testing.T) {
	home := sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled:   true,
		Escalated: []string{"docker"},
		Profiles: map[string]sandboxpolicy.Profile{
			"docker": {
				Mounts: []sandboxpolicy.Mount{
					{
						Path: filepath.Join(home, ".docker"),
						Mode: sandboxpolicy.ModeReadWrite,
						Type: sandboxpolicy.LevelEscalated,
					},
				},
				Sockets: []sandboxpolicy.Socket{{Path: "/var/run/docker.sock", Type: sandboxpolicy.LevelEscalated}},
			},
		},
	}}

	require.NoError(t, cfg.validateSandbox())
}

func TestValidateSandbox_RejectsUnknownProfile(t *testing.T) {
	sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{Enabled: true, Escalated: []string{"missing"}}}

	assert.ErrorContains(t, cfg.validateSandbox(), `unknown profile "missing"`)
}

func TestValidateSandbox_RejectsMalformedNetworkEntry(t *testing.T) {
	sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true,
		Profiles: map[string]sandboxpolicy.Profile{
			"db": {Network: []sandboxpolicy.Network{{
				Address: "host-loopback", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{70000},
				Type: sandboxpolicy.LevelEscalated,
			}}},
		},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "invalid port")

	cfg.Sandbox.Profiles["db"] = sandboxpolicy.Profile{Network: []sandboxpolicy.Network{{
		Address: "postgres.internal", Protocol: sandboxpolicy.ProtocolTCP, Ports: []int{5432},
		Type: sandboxpolicy.LevelEscalated,
	}}}

	assert.ErrorContains(t, cfg.validateSandbox(), "postgres.internal")
}

func TestValidateSandbox_RejectsBroadMount(t *testing.T) {
	sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true,
		Profiles: map[string]sandboxpolicy.Profile{
			"wide": {Mounts: []sandboxpolicy.Mount{{
				Path: "/", Mode: sandboxpolicy.ModeReadOnly, Type: sandboxpolicy.LevelBasic,
			}}},
		},
	}}

	assert.ErrorContains(t, cfg.validateSandbox(), "filesystem root")
}

func TestValidateSandbox_RejectsBroadLegacyWritablePath(t *testing.T) {
	sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{Enabled: true, WritablePaths: []string{"/tmp"}}}

	assert.ErrorContains(t, cfg.validateSandbox(), "sandbox.writable_paths")
}

func TestSandboxCatalog_MergesConfiguredProfiles(t *testing.T) {
	home := sandboxTestHome(t)

	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true,
		Profiles: map[string]sandboxpolicy.Profile{
			"local": {Mounts: []sandboxpolicy.Mount{
				{
					Path: filepath.Join(
						home,
						"local-data",
					),
					Mode: sandboxpolicy.ModeReadWrite,
					Type: sandboxpolicy.LevelBasic,
				},
			}},
		},
	}}

	catalog, err := cfg.SandboxCatalog()
	require.NoError(t, err)

	_, ok := catalog.Profile("local")
	assert.True(t, ok)
	_, ok = catalog.Profile("mise")
	assert.True(t, ok, "built-in definitions survive a configured addition")
}

func TestSandboxEnvironment_CapturesOnlyReviewedPathOverrides(t *testing.T) {
	sandboxTestHome(t)
	t.Setenv("MISE_INSTALLS_DIR", "/operator/mise-installs")
	t.Setenv("UNRELATED_SECRET", "fixture-secret")
	values := SandboxEnvironment()
	assert.Equal(t, "/operator/mise-installs", values["MISE_INSTALLS_DIR"])
	_, present := values["UNRELATED_SECRET"]
	assert.False(t, present)
}
