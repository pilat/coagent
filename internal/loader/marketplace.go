package loader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

const (
	pluginsDirName         = "plugins"
	pluginManifestDirName  = ".claude-plugin"
	pluginManifestFileName = "plugin.json"
)

// RepositoryResolver resolves a marketplace URL to a local path
// This abstraction allows for testing without real git operations
type RepositoryResolver func(ctx context.Context, url string) (string, error)

func parseGitHubURL(url string) (string, string, error) {
	url = strings.TrimSpace(url)
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimSuffix(url, "/")

	if !strings.HasPrefix(url, "github.com/") {
		return "", "", fmt.Errorf("invalid GitHub URL: %s (must be github.com/owner/repo)", url)
	}

	path := strings.TrimPrefix(url, "github.com/")

	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid GitHub URL format: %s (expected: owner/repo)", url)
	}

	owner := parts[0]
	repo := parts[1]

	if owner == "" || repo == "" {
		return "", "", errors.New("invalid GitHub URL: owner and repo cannot be empty")
	}

	return owner, repo, nil
}

func cacheDirForMarketplace(owner, repo string) (string, error) {
	dir, err := coagenthome.Join(coagenthome.CacheDirName, coagenthome.MarketplacesDirName, owner, repo)
	if err != nil {
		return "", fmt.Errorf("marketplace cache dir: %w", err)
	}

	return dir, nil
}

// ProcessMarketplaces processes every entry via ProcessMarketplace, logging and
// continuing past a failing entry so one bad marketplace can't block the rest.
func (s *svc) ProcessMarketplaces(ctx context.Context, entries []config.MarketplaceEntry, resolver RepositoryResolver) {
	for _, entry := range entries {
		if err := s.ProcessMarketplace(ctx, entry, resolver); err != nil {
			logger.Named("loader").Warn("processing_marketplace_failed", zap.String("url", entry.URL), zap.Error(err))
		}
	}
}

func (s *svc) ProcessMarketplace(
	ctx context.Context,
	entry config.MarketplaceEntry,
	resolver RepositoryResolver,
) error {
	if s.marketplaceCache != nil {
		return s.processMarketplaceWithCache(ctx, entry)
	}

	return s.processMarketplaceDirect(ctx, entry, resolver)
}

func (s *svc) processMarketplaceWithCache(ctx context.Context, entry config.MarketplaceEntry) error {
	skills, agents, err := s.marketplaceCache.Resolve(ctx, entry)
	if err != nil {
		return fmt.Errorf("resolving marketplace via cache: %w", err)
	}

	s.marketplaceSkillPaths = append(s.marketplaceSkillPaths, skills...)
	s.marketplaceAgentPaths = append(s.marketplaceAgentPaths, agents...)

	return nil
}

func (s *svc) processMarketplaceDirect(
	ctx context.Context,
	entry config.MarketplaceEntry,
	resolver RepositoryResolver,
) error {
	owner, repo, err := parseGitHubURL(entry.URL)
	if err != nil {
		return fmt.Errorf("parsing marketplace URL %s: %w", entry.URL, err)
	}

	cacheDir, err := cacheDirForMarketplace(owner, repo)
	if err != nil {
		return fmt.Errorf("getting cache directory: %w", err)
	}

	var repoPath string
	if resolver != nil {
		repoPath, err = resolver(ctx, entry.URL)
		if err != nil {
			return fmt.Errorf("resolving repository: %w", err)
		}
	} else {
		repoPath = cacheDir
	}

	for _, pluginName := range entry.Plugins {
		skills, agents, err := processPlugin(repoPath, pluginName)
		if err != nil {
			continue
		}

		s.marketplaceSkillPaths = append(s.marketplaceSkillPaths, skills...)
		s.marketplaceAgentPaths = append(s.marketplaceAgentPaths, agents...)
	}

	return nil
}

func processPlugin(repoPath, pluginName string) ([]sourceInfo, []sourceInfo, error) {
	// Try plugins/{pluginName} first (standard marketplace structure)
	pluginDir := filepath.Join(repoPath, pluginsDirName, pluginName)

	// Fallback to root if not found (alternative structure)
	if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
		pluginDir = filepath.Join(repoPath, pluginName)
	}

	info, err := os.Stat(pluginDir)
	if err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("plugin directory not found: %s", pluginDir)
	}

	manifestPath := filepath.Join(pluginDir, pluginManifestDirName, pluginManifestFileName)

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading plugin manifest: %w", err)
	}

	if _, err := parsePluginManifest(string(manifestData)); err != nil {
		return nil, nil, fmt.Errorf("parsing plugin manifest: %w", err)
	}

	var skills []sourceInfo

	skillsDir := filepath.Join(pluginDir, config.SkillsDirName)
	if si, err := os.Stat(skillsDir); err == nil && si.IsDir() {
		skills = []sourceInfo{{path: skillsDir, pluginName: pluginName}}
	}

	// Collect agents (try "agents" first, fallback to "commands")
	var agents []sourceInfo

	agentsDir := filepath.Join(pluginDir, config.AgentsDirName)
	if si, err := os.Stat(agentsDir); err == nil && si.IsDir() {
		agents = []sourceInfo{{path: agentsDir, pluginName: pluginName}}
	} else {
		commandsDir := filepath.Join(pluginDir, config.CommandsDirName)
		if si, err := os.Stat(commandsDir); err == nil && si.IsDir() {
			agents = []sourceInfo{{path: commandsDir, pluginName: pluginName}}
		}
	}

	return skills, agents, nil
}
