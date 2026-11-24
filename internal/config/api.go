package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	APIRoleOperator = "operator"
	APIRoleApp      = "app"
)

var (
	apiClientID  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	apiTokenHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// APIConfig declares the clients allowed to use the local API.
type APIConfig struct {
	// Listen opts in to serving the local API on a loopback IP address and port.
	Listen string `yaml:"listen,omitempty"`
	// Origins lists the browser origins allowed to call the loopback listener.
	Origins []string         `yaml:"origins,omitempty"`
	Clients []APIClientEntry `yaml:"clients,omitempty"`
}

// APIClientEntry identifies an API client and stores only its token hash.
type APIClientEntry struct {
	ID          string   `yaml:"id"`
	Role        string   `yaml:"role"`
	TokenSHA256 string   `yaml:"token_sha256"`
	Projects    []string `yaml:"projects,omitempty"`
}

// ValidAPIClientID reports whether id can name a local API client.
func ValidAPIClientID(id string) bool {
	return apiClientID.MatchString(id)
}

func (c *UnifiedConfig) validateAPI() error {
	if err := c.API.validateAPIListen(); err != nil {
		return err
	}

	if err := c.API.validateAPIOrigins(); err != nil {
		return err
	}

	ids := make(map[string]bool)
	hashes := make(map[string]string)

	for i, entry := range c.API.Clients {
		if entry.ID == "" {
			return fmt.Errorf("api client at index %d has no id specified", i)
		}

		if !ValidAPIClientID(entry.ID) {
			return fmt.Errorf("api client id %q must match ^[a-z0-9][a-z0-9_-]{0,31}$", entry.ID)
		}

		if ids[entry.ID] {
			return fmt.Errorf("duplicate api client id %q", entry.ID)
		}

		ids[entry.ID] = true

		if slices.ContainsFunc(c.Managers, func(manager ManagerEntry) bool { return manager.ID == entry.ID }) {
			return fmt.Errorf("api client id %q is also a manager id", entry.ID)
		}

		if entry.Role != APIRoleOperator && entry.Role != APIRoleApp {
			return fmt.Errorf("api client %q has unsupported role %q, must be: operator, app", entry.ID, entry.Role)
		}

		if entry.Projects != nil && entry.Role != APIRoleApp {
			return fmt.Errorf("api client %q: projects apply only to role app", entry.ID)
		}

		if entry.Projects != nil && len(entry.Projects) == 0 {
			return fmt.Errorf(
				"api client %q: projects must not be empty; omit projects to reach every project",
				entry.ID,
			)
		}

		projects := make(map[string]bool)

		for _, project := range entry.Projects {
			if !filepath.IsAbs(project) || filepath.Clean(project) != project {
				return fmt.Errorf("api client %q project %q must be a clean absolute path", entry.ID, project)
			}

			if projects[project] {
				return fmt.Errorf("api client %q lists project %q twice", entry.ID, project)
			}

			projects[project] = true
		}

		if !apiTokenHash.MatchString(entry.TokenSHA256) {
			return fmt.Errorf("api client %q token_sha256 must be 64 lowercase hex characters", entry.ID)
		}

		if previous, exists := hashes[entry.TokenSHA256]; exists {
			return fmt.Errorf("api clients %q and %q share a token", previous, entry.ID)
		}

		hashes[entry.TokenSHA256] = entry.ID
	}

	return nil
}
