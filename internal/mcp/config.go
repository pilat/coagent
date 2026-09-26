package mcp

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
