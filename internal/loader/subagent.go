package loader

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadSubagents finds and parses all subagent definition files.
// Local subagents take priority over global. .claude over .coagent.
// Marketplace subagents are prefixed with plugin name (e.g., "pilat:code-reviewer").
func (s *svc) LoadSubagents(workDir string) error {
	s.subagents = make(map[string]*Subagent)

	// Build search sources - lowest priority first
	// Later sources overwrite earlier ones
	searchSources := make([]sourceInfo, 0, len(s.marketplaceAgentPaths)+4)
	searchSources = append(searchSources, s.marketplaceAgentPaths...)
	searchSources = append(searchSources,
		sourceInfo{path: globalAgentsDir()},
		sourceInfo{path: projectCoagentAgentsDir(workDir)},
		sourceInfo{path: projectAgentsDir(workDir)},
	)

	// A broken source is skipped, not fatal: aborting the scan would silently drop
	// every higher-priority source behind it.
	var errs []error

	for _, src := range searchSources {
		if src.path == "" {
			continue
		}

		err := s.loadSubagentsFromPath(src.path, src.pluginName)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}

		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (s *svc) loadSubagentsFromPath(searchPath, pluginName string) error {
	entries, err := os.ReadDir(searchPath)
	if err != nil {
		return fmt.Errorf("scan subagent directory %s: %w", searchPath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		agentPath := filepath.Join(searchPath, entry.Name())

		agent, err := s.parseSubagentFile(agentPath)
		if err != nil {
			continue
		}

		if agent.Name == "" {
			agent.Name = strings.TrimSuffix(entry.Name(), ".md")
		}

		if pluginName != "" {
			agent.Name = pluginName + ":" + agent.Name
		}

		s.subagents[agent.Name] = agent
	}

	return nil
}

func (s *svc) parseSubagentFile(path string) (*Subagent, error) {
	frontmatter, content, err := parseFrontmatterFile(path)
	if err != nil {
		return nil, err
	}

	agent := &Subagent{
		Path:   path,
		Prompt: content,
	}

	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, agent); err != nil {
			return nil, fmt.Errorf("invalid frontmatter in %s: %w", path, err)
		}
	}

	return agent, nil
}

func (s *svc) GetSubagent(name string) *Subagent {
	return s.subagents[name]
}

// ListSubagents returns all loaded subagents sorted by name for stable ordering.
func (s *svc) ListSubagents() []*Subagent {
	subagents := make([]*Subagent, 0, len(s.subagents))
	for _, sa := range s.subagents {
		subagents = append(subagents, sa)
	}

	slices.SortFunc(subagents, func(a, b *Subagent) int { return cmp.Compare(a.Name, b.Name) })

	return subagents
}

// RegisterSubagent adds a subagent directly, bypassing filesystem discovery.
func (s *svc) RegisterSubagent(subagent *Subagent) {
	s.subagents[subagent.Name] = subagent
}
