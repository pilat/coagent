package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"

	"github.com/pilat/coagent/internal/coagenthome"
)

// minSecretValueLen filters out short strings whose redaction would mangle
// ordinary log text; real credentials are always longer.
const minSecretValueLen = 8

// secretRef matches a braced reference only: a bare $NAME form would mangle
// literal keys that contain '$'.
var secretRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Secrets holds credentials parsed from ~/.coagent/secrets. They are never
// exported into the process environment, so tool subprocesses cannot read them.
type Secrets map[string]string

// SecretsFilePath is where credentials live, beside the config.
func SecretsFilePath() (string, error) {
	path, err := coagenthome.Join(coagenthome.SecretsFileName)
	if err != nil {
		return "", fmt.Errorf("secrets file path: %w", err)
	}

	return path, nil
}

// LoadSecrets reads the default secrets file.
func LoadSecrets() (Secrets, error) {
	path, err := SecretsFilePath()
	if err != nil {
		return nil, err
	}

	return LoadSecretsFrom(path)
}

// LoadSecretsFrom reads a secrets file. A missing file yields an empty set; a
// malformed one is fatal, since silently losing every credential surfaces as
// unrelated "requires api_key" validation errors.
//
// This is the only door to the secrets file: godotenv is confined to this
// package so credentials cannot reach the process environment.
func LoadSecretsFrom(path string) (Secrets, error) {
	values, err := godotenv.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return Secrets{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read secrets file %q: %w", path, err)
	}

	return values, nil
}

// SecretValues collects every string the process treats as a credential, for log
// redaction.
func SecretValues(secrets Secrets, unified *UnifiedConfig) []string {
	return secretValues(secrets, unified)
}

// Environment overlays the real environment on top of the secrets, matching the
// precedence godotenv applied before secrets stopped reaching the environment.
func (s Secrets) Environment() map[string]string {
	merged := make(map[string]string, len(s))
	maps.Copy(merged, s)
	maps.Copy(merged, env.ToMap(os.Environ()))

	return merged
}

// resolveSecrets substitutes ${VAR} references in the fields allowed to carry a
// credential. Every other field stays literal — adding a sink is a code change
// by design.
func (c *UnifiedConfig) resolveSecrets(secrets Secrets) error {
	for name, provider := range c.Providers {
		apiKey, err := secrets.Expand(provider.APIKey)
		if err != nil {
			return fmt.Errorf("provider %q api_key: %w", name, err)
		}

		provider.APIKey = apiKey
		c.Providers[name] = provider
	}

	for i := range c.Managers {
		botToken, err := secrets.Expand(c.Managers[i].BotToken)
		if err != nil {
			return fmt.Errorf("manager %q bot_token: %w", c.Managers[i].ID, err)
		}

		c.Managers[i].BotToken = botToken
	}

	if c.Tools.Search.APIKey != "" {
		apiKey, err := secrets.Expand(c.Tools.Search.APIKey)
		if err != nil {
			return fmt.Errorf("tools.search api_key: %w", err)
		}

		c.Tools.Search.APIKey = apiKey
	}

	return nil
}

// secretValues collects every string the process treats as a credential:
// secrets-file values plus the resolved api_key/bot_token sinks (which may be
// inline literals). MCP server env is resolved at acquire time from these same
// secrets-file values, so it needs no separate pass here.
func secretValues(secrets Secrets, unified *UnifiedConfig) []string {
	seen := make(map[string]struct{})

	var values []string

	add := func(v string) {
		if len(v) < minSecretValueLen {
			return
		}

		if _, ok := seen[v]; ok {
			return
		}

		seen[v] = struct{}{}
		values = append(values, v)
	}

	for _, v := range secrets {
		add(v)
	}

	if unified == nil {
		return values
	}

	for _, provider := range unified.Providers {
		add(provider.APIKey)
	}

	for _, manager := range unified.Managers {
		add(manager.BotToken)
	}

	add(unified.Tools.Search.APIKey)

	return values
}

// Expand substitutes ${VAR} references from the secrets map. An undefined name is
// an error naming the variable — never its value.
func (s Secrets) Expand(value string) (string, error) {
	var missing []string

	expanded := secretRef.ReplaceAllStringFunc(value, func(match string) string {
		name := match[2 : len(match)-1]

		resolved, ok := s[name]
		if !ok {
			missing = append(missing, name)

			return match
		}

		return resolved
	})

	if len(missing) > 0 {
		return "", fmt.Errorf(
			"undefined %s: define it in ~/%s/%s",
			strings.Join(missing, ", "),
			coagenthome.DirName,
			coagenthome.SecretsFileName,
		)
	}

	return expanded, nil
}
