package loader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

type mockPlugin struct {
	manifest pluginManifest
	skills   []mockSkill
	agents   []mockAgent
}

type mockSkill struct {
	name    string
	content string
}

type mockAgent struct {
	name    string
	content string
}

func TestSvc_ProcessMarketplace_Success(t *testing.T) {
	isolateMarketplaceHome(t)

	marketplaceDir := createMockMarketplaceRepo(t, "test-owner", "test-repo", map[string]mockPlugin{
		"test-plugin": {
			manifest: pluginManifest{
				Name:        "test-plugin",
				Version:     "1.0.0",
				Description: "Test plugin",
			},
			skills: []mockSkill{
				{name: "test-skill", content: "# Test Skill"},
			},
			agents: []mockAgent{
				{name: "test-agent", content: "# Test Agent"},
			},
		},
	})

	entry := config.MarketplaceEntry{
		URL:     "github.com/test-owner/test-repo",
		Plugins: []string{"test-plugin"},
	}

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), entry, func(_ context.Context, url string) (string, error) {
		return marketplaceDir, nil
	})
	require.NoError(t, err)

	assert.Len(t, svc.marketplaceSkillPaths, 1)
	assert.Len(t, svc.marketplaceAgentPaths, 1)
}

func TestSvc_ProcessMarketplace_InvalidURL(t *testing.T) {
	isolateMarketplaceHome(t)

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), config.MarketplaceEntry{
		URL:     "gitlab.com/test-owner/test-repo",
		Plugins: []string{"test-plugin"},
	}, nil)
	require.Error(t, err)
}

func TestSvc_ProcessMarketplace_ResolverError(t *testing.T) {
	isolateMarketplaceHome(t)

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), config.MarketplaceEntry{
		URL:     "github.com/test-owner/test-repo",
		Plugins: []string{"test-plugin"},
	}, func(context.Context, string) (string, error) {
		return "", assert.AnError
	})
	require.Error(t, err)
}

func TestSvc_ProcessMarketplace_InvalidPluginManifest(t *testing.T) {
	isolateMarketplaceHome(t)

	marketplaceDir := t.TempDir()
	pluginDir := filepath.Join(marketplaceDir, "test-plugin", pluginManifestDirName)
	require.NoError(t, os.MkdirAll(pluginDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(pluginDir, pluginManifestFileName),
		[]byte("{invalid json"),
		0o644,
	))

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), config.MarketplaceEntry{
		URL:     "github.com/test-owner/test-repo",
		Plugins: []string{"test-plugin"},
	}, func(_ context.Context, url string) (string, error) {
		return marketplaceDir, nil
	})

	require.NoError(t, err)
	assert.Empty(t, svc.marketplaceSkillPaths)
}

func TestSvc_ProcessMarketplace_PluginNotInConfig(t *testing.T) {
	isolateMarketplaceHome(t)

	marketplaceDir := createMockMarketplaceRepo(t, "test-owner", "test-repo", map[string]mockPlugin{
		"enabled-plugin": {
			manifest: pluginManifest{Name: "enabled-plugin", Version: "1.0.0"},
			skills:   []mockSkill{{name: "enabled-skill", content: "# Enabled"}},
		},
		"disabled-plugin": {
			manifest: pluginManifest{Name: "disabled-plugin", Version: "1.0.0"},
			skills:   []mockSkill{{name: "disabled-skill", content: "# Disabled"}},
		},
	})

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), config.MarketplaceEntry{
		URL:     "github.com/test-owner/test-repo",
		Plugins: []string{"enabled-plugin"},
	}, func(_ context.Context, url string) (string, error) {
		return marketplaceDir, nil
	})
	require.NoError(t, err)

	assert.Len(t, svc.marketplaceSkillPaths, 1)
	assert.Contains(t, svc.marketplaceSkillPaths[0].path, "enabled-plugin")
	assert.Equal(t, "enabled-plugin", svc.marketplaceSkillPaths[0].pluginName)
}

func TestSvc_ProcessMarketplace_LocalPriority(t *testing.T) {
	isolateMarketplaceHome(t)

	marketplaceDir := createMockMarketplaceRepo(t, "owner", "repo", map[string]mockPlugin{
		"plugin": {
			manifest: pluginManifest{Name: "plugin", Version: "1.0.0"},
			skills:   []mockSkill{{name: "marketplace-skill", content: "# Marketplace"}},
		},
	})

	tempDir := t.TempDir()
	skillsDir := filepath.Join(tempDir, config.ProjectConfigDir, config.SkillsDirName, "marketplace-skill")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillsDir, config.SkillFileName),
		[]byte("# Local Skill"),
		0o644,
	))

	svc := New().(*svc)
	err := svc.ProcessMarketplace(context.Background(), config.MarketplaceEntry{
		URL:     "github.com/owner/repo",
		Plugins: []string{"plugin"},
	}, func(_ context.Context, url string) (string, error) {
		return marketplaceDir, nil
	})
	require.NoError(t, err)

	err = svc.LoadSkills(tempDir)
	require.NoError(t, err)

	skill := svc.GetSkill("marketplace-skill")
	require.NotNil(t, skill)
	assert.Contains(t, skill.Content, "Local")
}

func TestProcessPlugin_SkillCollection(t *testing.T) {
	repoDir := t.TempDir()
	pluginDir := filepath.Join(repoDir, "test-plugin")

	manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
	require.NoError(t, os.MkdirAll(manifestDir, 0o755))
	manifest := pluginManifest{Name: "test-plugin", Version: "1.0.0"}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, pluginManifestFileName), data, 0o644))

	skillsDir := filepath.Join(pluginDir, config.SkillsDirName)
	skill1Dir := filepath.Join(skillsDir, "skill-one")
	skill2Dir := filepath.Join(skillsDir, "skill-two")

	require.NoError(t, os.MkdirAll(skill1Dir, 0o755))
	require.NoError(t, os.MkdirAll(skill2Dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skill1Dir, config.SkillFileName), []byte("# Skill One"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(skill2Dir, config.SkillFileName), []byte("# Skill Two"), 0o644))

	skills, _, err := processPlugin(repoDir, "test-plugin")
	require.NoError(t, err)
	assert.Len(t, skills, 1)
	assert.Equal(t, skillsDir, skills[0].path)
	assert.Equal(t, "test-plugin", skills[0].pluginName)
}

func TestProcessPlugin_AgentCollection(t *testing.T) {
	repoDir := t.TempDir()
	pluginDir := filepath.Join(repoDir, "test-plugin")

	manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
	require.NoError(t, os.MkdirAll(manifestDir, 0o755))
	manifest := pluginManifest{Name: "test-plugin", Version: "1.0.0"}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, pluginManifestFileName), data, 0o644))

	agentsDir := filepath.Join(pluginDir, config.AgentsDirName)
	require.NoError(t, os.MkdirAll(agentsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "agent-one.md"), []byte("# Agent One"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(agentsDir, "agent-two.md"), []byte("# Agent Two"), 0o644))

	_, agents, err := processPlugin(repoDir, "test-plugin")
	require.NoError(t, err)
	assert.Len(t, agents, 1)
	assert.Equal(t, agentsDir, agents[0].path)
	assert.Equal(t, "test-plugin", agents[0].pluginName)
}

func TestProcessPlugin_CommandsDirFallback(t *testing.T) {
	repoDir := t.TempDir()
	pluginDir := filepath.Join(repoDir, "test-plugin")

	manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
	require.NoError(t, os.MkdirAll(manifestDir, 0o755))
	manifest := pluginManifest{Name: "test-plugin", Version: "1.0.0"}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, pluginManifestFileName), data, 0o644))

	commandsDir := filepath.Join(pluginDir, config.CommandsDirName)
	require.NoError(t, os.MkdirAll(commandsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(commandsDir, "command.md"), []byte("# Command"), 0o644))

	_, agents, err := processPlugin(repoDir, "test-plugin")
	require.NoError(t, err)
	assert.Len(t, agents, 1)
	assert.Equal(t, commandsDir, agents[0].path)
	assert.Equal(t, "test-plugin", agents[0].pluginName)
}

func isolateMarketplaceHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func createMockMarketplaceRepo(t *testing.T, _ string, _ string, plugins map[string]mockPlugin) string {
	t.Helper()
	repoDir := t.TempDir()

	for name, plugin := range plugins {
		pluginDir := filepath.Join(repoDir, name)

		manifestDir := filepath.Join(pluginDir, pluginManifestDirName)
		require.NoError(t, os.MkdirAll(manifestDir, 0o755))

		manifestData, err := json.Marshal(plugin.manifest)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(
			filepath.Join(manifestDir, pluginManifestFileName),
			manifestData,
			0o644,
		))

		if len(plugin.skills) > 0 {
			skillsDir := filepath.Join(pluginDir, config.SkillsDirName)
			for _, skill := range plugin.skills {
				skillDir := filepath.Join(skillsDir, skill.name)
				require.NoError(t, os.MkdirAll(skillDir, 0o755))
				require.NoError(t, os.WriteFile(
					filepath.Join(skillDir, config.SkillFileName),
					[]byte(skill.content),
					0o644,
				))
			}
		}

		if len(plugin.agents) > 0 {
			agentsDir := filepath.Join(pluginDir, config.AgentsDirName)
			require.NoError(t, os.MkdirAll(agentsDir, 0o755))
			for _, agent := range plugin.agents {
				require.NoError(t, os.WriteFile(
					filepath.Join(agentsDir, agent.name+".md"),
					[]byte(agent.content),
					0o644,
				))
			}
		}
	}

	return repoDir
}
