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

	"github.com/pilat/coagent/internal/config"
)

// LoadSkills finds and parses all SKILL.md files from search paths.
// Local skills take priority over global skills. .claude over .coagent.
// Marketplace skills are prefixed with plugin name (e.g., "pilat:brainstormer").
func (s *svc) LoadSkills(workDir string) error {
	s.skills = make(map[string]*Skill)

	// Build search sources - lowest priority first
	// Later sources overwrite earlier ones
	searchSources := make([]sourceInfo, 0, len(s.marketplaceSkillPaths)+5)
	searchSources = append(searchSources, s.marketplaceSkillPaths...)

	if gsd := globalSkillsDir(); gsd != "" {
		searchSources = append(searchSources, sourceInfo{path: gsd})
	}

	searchSources = append(searchSources,
		sourceInfo{path: projectAgentsSkillsDir(workDir)},
		sourceInfo{path: projectCoagentSkillsDir(workDir)},
		sourceInfo{path: projectCommandsDir(workDir)},
		sourceInfo{path: projectSkillsDir(workDir)},
	)

	// A broken source is skipped, not fatal: aborting the scan would silently drop
	// every higher-priority source behind it.
	var errs []error

	for _, src := range searchSources {
		if src.path == "" {
			continue
		}

		err := s.loadSkillsFromPath(src.path, src.pluginName)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}

		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (s *svc) loadSkillsFromPath(searchPath, pluginName string) error {
	entries, err := os.ReadDir(searchPath)
	if err != nil {
		return fmt.Errorf("scan skill directory %s: %w", searchPath, err)
	}

	for _, entry := range entries {
		skill, skillName, ok := s.parseSkillEntry(searchPath, entry)
		if !ok {
			continue
		}

		if pluginName != "" {
			skillName = pluginName + ":" + skillName
		}

		skill.Name = skillName
		s.skills[skillName] = skill
	}

	return nil
}

func (s *svc) parseSkillEntry(searchPath string, entry os.DirEntry) (*Skill, string, bool) {
	isDir := entry.IsDir()
	if entry.Type()&os.ModeSymlink != 0 {
		if info, err := os.Stat(filepath.Join(searchPath, entry.Name())); err == nil {
			isDir = info.IsDir()
		}
	}

	if isDir {
		skillPath := filepath.Join(searchPath, entry.Name(), config.SkillFileName)

		skill, err := s.parseSkillFile(skillPath)
		if err != nil {
			return nil, "", false
		}

		skillName := entry.Name()
		if skill.Name != "" {
			skillName = skill.Name
		}

		return skill, skillName, true
	}

	if !strings.HasSuffix(entry.Name(), ".md") {
		return nil, "", false
	}

	skillPath := filepath.Join(searchPath, entry.Name())

	skill, err := s.parseSkillFile(skillPath)
	if err != nil {
		return nil, "", false
	}

	skillName := strings.TrimSuffix(entry.Name(), ".md")
	if skill.Name != "" {
		skillName = skill.Name
	}

	return skill, skillName, true
}

func (s *svc) parseSkillFile(path string) (*Skill, error) {
	frontmatter, content, err := parseFrontmatterFile(path)
	if err != nil {
		return nil, err
	}

	skill := &Skill{
		Path:    path,
		Content: content,
	}

	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, skill); err != nil {
			return nil, fmt.Errorf("invalid frontmatter in %s: %w", path, err)
		}
	}

	return skill, nil
}

func (s *svc) GetSkill(name string) *Skill {
	return s.skills[name]
}

// ListSkills returns all loaded skills sorted by name for stable ordering.
func (s *svc) ListSkills() []*Skill {
	skills := make([]*Skill, 0, len(s.skills))
	for _, sk := range s.skills {
		skills = append(skills, sk)
	}

	slices.SortFunc(skills, func(a, b *Skill) int { return cmp.Compare(a.Name, b.Name) })

	return skills
}

func (s *svc) ListUserInvocableSkills() []*Skill {
	skills := make([]*Skill, 0)
	for _, sk := range s.skills {
		if sk.IsUserInvocable() {
			skills = append(skills, sk)
		}
	}

	slices.SortFunc(skills, func(a, b *Skill) int { return cmp.Compare(a.Name, b.Name) })

	return skills
}

// ListModelInvocableSkills returns model-visible skills sorted by name.
func (s *svc) ListModelInvocableSkills() []*Skill {
	skills := make([]*Skill, 0)
	for _, sk := range s.skills {
		if sk.IsModelInvocable() {
			skills = append(skills, sk)
		}
	}

	slices.SortFunc(skills, func(a, b *Skill) int { return cmp.Compare(a.Name, b.Name) })

	return skills
}

// RegisterSkill adds a skill directly, bypassing filesystem discovery.
func (s *svc) RegisterSkill(skill *Skill) {
	s.skills[skill.Name] = skill
}
