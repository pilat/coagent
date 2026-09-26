package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// validateSandbox rejects a candidate sandbox section whose profiles, paths,
// escalation names, network entries or legacy grants cannot be compiled. It
// runs on boot and on every staged candidate, so an invalid definition never
// replaces the live configuration.
func (c *UnifiedConfig) validateSandbox() error {
	if err := sandboxpolicy.ValidateSectionWithEnvironment(c.Sandbox, SandboxEnvironment()); err != nil {
		return fmt.Errorf("validate sandbox section: %w", err)
	}

	return nil
}

// SandboxEnvironment captures only the reviewed path overrides from the daemon
// environment; the policy compiler receives no ambient process authority.
func SandboxEnvironment() map[string]string {
	allowed := make(map[string]struct{})
	for _, name := range sandboxpolicy.CatalogEnvironmentNames() {
		allowed[name] = struct{}{}
	}

	values := make(map[string]string)

	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}

		if _, permitted := allowed[name]; permitted {
			values[name] = value
		}
	}

	return values
}

// SandboxCatalog resolves the built-in catalog merged with the configured
// profile definitions. Callers compile effective policies from it.
func (c *UnifiedConfig) SandboxCatalog() (sandboxpolicy.Catalog, error) {
	catalog, err := sandboxpolicy.Load(c.Sandbox.Profiles)
	if err != nil {
		return sandboxpolicy.Catalog{}, fmt.Errorf("load sandbox catalog: %w", err)
	}

	return catalog, nil
}
