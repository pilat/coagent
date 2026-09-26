package sandboxpolicy

import (
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	envXDGConfigHome = "XDG_CONFIG_HOME"
	envMiseDataDir   = "MISE_DATA_DIR"
	miseDirName      = "mise"
)

// catalogEnvDefaults is the finite set of daemon-start environment names the
// shipped catalog may reference. A name with an empty default drops its entry
// when unset; there is no PATH-derived or captured-environment expansion.
var catalogEnvDefaults = map[string]string{
	"SSH_AUTH_SOCK":     "",
	"GH_CONFIG_DIR":     "~/.config/gh",
	envXDGConfigHome:    "~/.config",
	"XDG_DATA_HOME":     "~/.local/share",
	"XDG_CACHE_HOME":    "~/.cache",
	"XDG_STATE_HOME":    "~/.local/state",
	"MISE_CONFIG_DIR":   "",
	"MISE_CACHE_DIR":    "",
	"MISE_STATE_DIR":    "",
	envMiseDataDir:      "",
	"MISE_INSTALLS_DIR": "",
}

//go:embed catalog.yaml
var catalogData []byte

// Catalog is the reviewed built-in profile set merged with operator
// definitions. A configured definition replaces a built-in one in full; a new
// name adds a profile.
type Catalog struct {
	version  string
	profiles map[string]Profile
}

type catalogFile struct {
	Version  string             `json:"version"  yaml:"version"`
	Profiles map[string]Profile `json:"profiles" yaml:"profiles"`
}

// Load parses the embedded catalog and applies operator overrides.
func Load(overrides map[string]Profile) (Catalog, error) {
	var file catalogFile
	if err := yaml.Unmarshal(catalogData, &file); err != nil {
		return Catalog{}, fmt.Errorf("parse built-in sandbox catalog: %w", err)
	}

	if file.Version == "" {
		return Catalog{}, errors.New("built-in sandbox catalog has no version")
	}

	profiles := make(map[string]Profile, len(file.Profiles)+len(overrides))

	for name, profile := range file.Profiles {
		if err := validateProfile(name, profile); err != nil {
			return Catalog{}, fmt.Errorf("built-in profile %q: %w", name, err)
		}

		profiles[name] = profile
	}

	for name, profile := range overrides {
		if name == "" {
			return Catalog{}, errors.New("sandbox profile name must not be empty")
		}

		if err := validateProfile(name, profile); err != nil {
			return Catalog{}, err
		}

		profiles[name] = profile
	}

	return Catalog{version: file.Version, profiles: profiles}, nil
}

// Version reports the embedded catalog revision, which is part of the
// effective policy digest.
func (c Catalog) Version() string { return c.version }

// Profile returns one merged profile definition.
func (c Catalog) Profile(name string) (Profile, bool) {
	profile, ok := c.profiles[name]

	return profile, ok
}

// Names lists every known profile in deterministic order.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c.profiles))
	for name := range c.profiles {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// CatalogEnvironmentNames returns the reviewed environment names whose values
// may affect built-in profile paths.
func CatalogEnvironmentNames() []string {
	names := make([]string, 0, len(catalogEnvDefaults))
	for name := range catalogEnvDefaults {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// catalogEnvValue reads one allowlisted daemon-start environment name, falling
// back to its documented default.
func catalogEnvValue(name string, values map[string]string) (string, bool) {
	defaultValue, ok := catalogEnvDefaults[name]
	if !ok {
		return "", false
	}

	if value := values[name]; value != "" {
		return value, true
	}

	derived := map[string]struct{ base, suffix string }{
		"GH_CONFIG_DIR":     {"XDG_CONFIG_HOME", "gh"},
		"MISE_CONFIG_DIR":   {envXDGConfigHome, miseDirName},
		"MISE_CACHE_DIR":    {"XDG_CACHE_HOME", miseDirName},
		"MISE_STATE_DIR":    {"XDG_STATE_HOME", miseDirName},
		envMiseDataDir:      {"XDG_DATA_HOME", miseDirName},
		"MISE_INSTALLS_DIR": {envMiseDataDir, "installs"},
	}
	if rule, ok := derived[name]; ok {
		base, ok := catalogEnvValue(rule.base, values)
		if !ok {
			return "", false
		}

		return filepath.Join(base, rule.suffix), true
	}

	return defaultValue, true
}
