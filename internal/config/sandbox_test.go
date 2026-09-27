package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func TestValidateSandbox_DocumentedExamples(t *testing.T) {
	data, err := os.ReadFile("../../docs/sandbox-boundary.md")
	require.NoError(t, err)
	blocks := strings.Split(string(data), "```yaml\n")
	require.Greater(t, len(blocks), 1)
	for _, block := range blocks[1:] {
		configYAML, _, found := strings.Cut(block, "```")
		require.True(t, found)
		var cfg UnifiedConfig
		decoder := yaml.NewDecoder(strings.NewReader(configYAML))
		decoder.KnownFields(true)
		require.NoError(t, decoder.Decode(&cfg))
		require.NoError(t, cfg.validateSandbox())
	}
}

func TestValidateSandbox_AcceptsAllowAndDenyRules(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true,
		Rules: []sandboxpolicy.Rule{
			{Deny: "~/.config/secret-tool"},
			{Deny: "/srv/credentials"},
			{Allow: "~/.cache/build", Mode: sandboxpolicy.ModeReadWrite},
			{Allow: "/tmp/shared-cache", Mode: sandboxpolicy.ModeReadWrite},
		},
	}}

	require.NoError(t, cfg.validateSandbox())
}

func TestValidateSandbox_RejectsDeniedTraversal(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true, Rules: []sandboxpolicy.Rule{{Deny: "~/../etc"}},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "traversal component")
}

func TestValidateSandbox_RejectsRelativeDeniedPath(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true, Rules: []sandboxpolicy.Rule{{Deny: "relative/secrets"}},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "must be absolute")
}

func TestValidateSandbox_RejectsRuleWithNeitherAllowNorDeny(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true, Rules: []sandboxpolicy.Rule{{}},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "neither allow nor deny")
}

func TestValidateSandbox_RejectsModeOnADenyRule(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true, Rules: []sandboxpolicy.Rule{{Deny: "/srv/data", Mode: sandboxpolicy.ModeReadWrite}},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "only valid on an allow rule")
}

func TestValidateSandbox_RejectsDuplicateProjectKeys(t *testing.T) {
	cfg := &UnifiedConfig{Sandbox: SandboxConfig{
		Enabled: true,
		Projects: map[string]sandboxpolicy.ProjectRules{
			"/srv/project":  {},
			"/srv/project/": {},
		},
	}}

	require.ErrorContains(t, cfg.validateSandbox(), "name the same project")
}
