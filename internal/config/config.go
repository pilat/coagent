package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/caarlos0/env/v11"
)

// Directory and file name constants
const (
	ProjectConfigDir   = ".claude"
	ProjectCoagentDir  = ".coagent"
	ContextFileName    = "CLAUDE.md"
	AgentsFileName     = "AGENTS.md"
	LocalContextSuffix = ".local"
	SkillsDirName      = "skills"
	AgentsDirName      = "agents"
	CommandsDirName    = "commands"
	AgentsConfigDir    = ".agents"
	SkillFileName      = "SKILL.md"
)

type (
	Config struct {
		Model string // current session model — resolved from UnifiedConfig.Models[0] at runtime

		// CLI-only fields (not loaded from env, set by main.go)
		WorkDir   string
		Prompt    string
		Resume    bool
		SessionID int64

		// RepoRoot is the path to the main git repository (for worktree sessions).
		// Empty for non-worktree sessions.
		RepoRoot string
		// GitDir is the path to the git directory file (for worktree sessions).
		// Empty for non-worktree sessions.
		GitDir string

		// Unified config (loaded from ~/.coagent/config.yaml)
		UnifiedConfig *UnifiedConfig

		// SecretValues holds every resolved credential string, for log redaction.
		SecretValues []string
	}
)

// NewConfig loads secrets, environment and unified YAML config. Secrets stay in
// memory, and are returned rather than stored on Config — a struct half the
// codebase holds would put every credential one field access from a log line.
func NewConfig() (*Config, Secrets, error) {
	secrets, err := LoadSecrets()
	if err != nil {
		return nil, nil, err
	}

	var cfg Config
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: secrets.Environment()}); err != nil {
		return nil, nil, fmt.Errorf("parse environment config: %w", err)
	}

	// A missing config file is fine (UnifiedConfig stays nil); main logs the outcome.
	unifiedCfg, err := LoadUnifiedConfig(DefaultUnifiedConfigFile, secrets)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	cfg.UnifiedConfig = unifiedCfg
	cfg.SecretValues = secretValues(secrets, unifiedCfg)

	return &cfg, secrets, nil
}

// DefaultModel returns the first model ID from UnifiedConfig, or "" if none configured.
func (c *Config) DefaultModel() string {
	if c.UnifiedConfig != nil && len(c.UnifiedConfig.Models) > 0 {
		return c.UnifiedConfig.Models[0].ID
	}

	return ""
}
