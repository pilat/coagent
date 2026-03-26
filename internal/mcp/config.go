package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

type Config struct {
	Servers map[string]ServerConfig `json:"servers"`
}

type ServerConfig struct {
	Command  string            `json:"command"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	WorkDir  string            `json:"work_dir,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	Enabled  *bool             `json:"enabled,omitempty"`
}

// Hash returns a deterministic SHA-256 hex digest of the server config.
// It includes Command, Args (order-preserving), Env (sorted key=value), and WorkDir.
// Disabled/Enabled fields are excluded — two configs that differ only
// in enabled state produce the same hash.
func (c ServerConfig) Hash() string {
	envPairs := make([]string, 0, len(c.Env))
	for k, v := range c.Env {
		envPairs = append(envPairs, fmt.Sprintf("%s=%s", k, v))
	}

	sort.Strings(envPairs)

	raw := strings.Join([]string{
		c.Command,
		strings.Join(c.Args, "\x00"),
		strings.Join(envPairs, "\x00"),
		c.WorkDir,
	}, "\x1f")

	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}

// IsEnabled returns true if the server should be started.
// Logic: if disabled=true → false; if enabled=false → false; otherwise true.
func (c ServerConfig) IsEnabled() bool {
	if c.Disabled {
		return false
	}

	if c.Enabled != nil && !*c.Enabled {
		return false
	}

	return true
}
