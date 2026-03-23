package loader

import (
	"path/filepath"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func globalDir() string {
	dir, err := coagenthome.Dir()
	if err != nil {
		return ""
	}

	return dir
}

func projectDir(cwd string) string {
	return filepath.Join(cwd, config.ProjectConfigDir)
}

func globalSkillsDir() string {
	gd := globalDir()
	if gd == "" {
		return ""
	}

	return filepath.Join(gd, config.SkillsDirName)
}

func projectSkillsDir(cwd string) string {
	return filepath.Join(projectDir(cwd), config.SkillsDirName)
}

func globalAgentsDir() string {
	gd := globalDir()
	if gd == "" {
		return ""
	}

	return filepath.Join(gd, config.AgentsDirName)
}

func projectAgentsDir(cwd string) string {
	return filepath.Join(projectDir(cwd), config.AgentsDirName)
}

func projectCoagentSkillsDir(cwd string) string {
	return filepath.Join(cwd, config.ProjectCoagentDir, config.SkillsDirName)
}

func projectCoagentAgentsDir(cwd string) string {
	return filepath.Join(cwd, config.ProjectCoagentDir, config.AgentsDirName)
}

func projectCommandsDir(cwd string) string {
	return filepath.Join(projectDir(cwd), config.CommandsDirName)
}

func projectAgentsSkillsDir(cwd string) string {
	return filepath.Join(cwd, config.AgentsConfigDir, config.SkillsDirName)
}

// contextFilePaths returns the ordered list of constitution file paths to search.
// Order: global AGENTS -> project AGENTS -> project CLAUDE -> local CLAUDE
func contextFilePaths(cwd string) []string {
	var paths []string

	if gd := globalDir(); gd != "" {
		paths = append(paths, filepath.Join(gd, config.AgentsFileName))
	}

	paths = append(paths,
		filepath.Join(cwd, config.AgentsFileName),
		filepath.Join(cwd, config.ContextFileName),
		filepath.Join(projectDir(cwd), config.ContextFileName),
	)

	localFileName := "CLAUDE" + config.LocalContextSuffix + ".md"
	paths = append(paths, filepath.Join(cwd, localFileName))

	return paths
}
