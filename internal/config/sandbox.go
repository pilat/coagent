package config

import (
	"fmt"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// validateSandbox rejects a candidate sandbox section whose rules cannot be
// compiled. It runs on boot and on every staged candidate, so an invalid
// section never replaces the live configuration.
func (c *UnifiedConfig) validateSandbox() error {
	if err := sandboxpolicy.ValidateSection(c.Sandbox); err != nil {
		return fmt.Errorf("validate sandbox section: %w", err)
	}

	return nil
}
