package config

import (
	"fmt"
	"regexp"
	"slices"
)

const APIRoleOperator = "operator"

var (
	apiClientID  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	apiTokenHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// APIConfig declares the clients allowed to use the local API.
type APIConfig struct {
	Clients []APIClientEntry `yaml:"clients,omitempty"`
}

// APIClientEntry identifies an API owner and stores only its token hash.
type APIClientEntry struct {
	ID          string `yaml:"id"`
	Role        string `yaml:"role"`
	TokenSHA256 string `yaml:"token_sha256"`
}

// ValidAPIClientID reports whether id can name a local API owner.
func ValidAPIClientID(id string) bool {
	return apiClientID.MatchString(id)
}

func (c *UnifiedConfig) validateAPI() error {
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

		if entry.Role != APIRoleOperator {
			return fmt.Errorf("api client %q has unsupported role %q, must be: operator", entry.ID, entry.Role)
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
