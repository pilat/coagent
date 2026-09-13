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

// globalContextCandidates returns the ordered candidate paths for the global
// context artifact. Empty when the user home cannot be resolved.
func globalContextCandidates() []string {
	home, err := coagenthome.UserHome()
	if err != nil {
		return nil
	}

	return []string{
		filepath.Join(home, coagenthome.DirName, config.AgentsFileName),
		filepath.Join(home, ".claude", config.ContextFileName),
		filepath.Join(home, ".codex", config.AgentsFileName),
		filepath.Join(home, ".config", "opencode", config.AgentsFileName),
		filepath.Join(home, ".gemini", "GEMINI.md"),
	}
}

// projectContextCandidates returns the ordered candidate paths for the project
// context artifact.
func projectContextCandidates(workDir string) []string {
	return []string{
		filepath.Join(workDir, config.AgentsFileName),
		filepath.Join(workDir, config.ContextFileName),
		filepath.Join(projectDir(workDir), config.ContextFileName),
	}
}

// localContextCandidates returns the ordered candidate paths for the local
// context artifact.
func localContextCandidates(workDir string) []string {
	return []string{
		filepath.Join(workDir, config.AgentsLocalFileName),
		filepath.Join(workDir, config.ContextLocalFileName),
	}
}
