package configops

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/pilat/coagent/internal/config"
)

// refOnlyMessage is what a rejected credential field is told. There is no
// token-detection heuristic: anything that is not a reference is refused, so a
// value can never reach config.yaml by looking innocent.
const refOnlyMessage = "must be a ${VAR} reference to a secret, not a value"

// secretRef matches the braced reference form the config loader resolves.
var secretRef = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// keylessDrivers are the provider drivers whose schema does not demand an
// api_key, so an empty one is legal for a brand-new provider.
var keylessDrivers = map[string]bool{"google-sa": true}

// Op is one semantic mutation of the raw config draft — a typed operation, never
// a YAML blob, so validation, guards and secret discipline live in one place.
//
// An op never carries a credential value: credentials reach config only as
// ${VAR} references to entries already in the secrets file.
type Op interface {
	// Path names the config location the op touches, for verdict anchoring.
	Path() string
	// Summary is the one line that describes the change in a verdict and in the
	// pending-apply marker.
	Summary() string

	apply(draft *config.UnifiedConfig) error
}

// SetProvider adds or replaces a provider. An empty api_key on an *existing*
// provider keeps whatever reference it already had; on a new one it is refused
// for drivers whose schema requires a key.
func SetProvider(name string, entry config.ProviderEntry) Op {
	return &setProvider{name: name, entry: entry}
}

// RemoveProvider deletes a provider. It refuses to remove the last one, or one
// any model still references — no cascade: a mutation that silently deletes
// models the caller did not name is worse than a refusal it can read.
func RemoveProvider(name string) Op { return &removeProvider{name: name} }

type setProvider struct {
	name  string
	entry config.ProviderEntry
}

func (o *setProvider) Path() string { return "providers." + o.name }

func (o *setProvider) Summary() string { return "set provider " + o.name }

func (o *setProvider) apply(draft *config.UnifiedConfig) error {
	if strings.TrimSpace(o.name) == "" {
		return errors.New("provider name is required")
	}

	if err := checkCredential("api_key", o.entry.APIKey); err != nil {
		return err
	}

	existing, exists := draft.Providers[o.name]

	entry := o.entry
	if entry.APIKey == "" {
		entry.APIKey = existing.APIKey
	}

	if !exists && entry.APIKey == "" && !keylessDrivers[entry.Driver] {
		return fmt.Errorf("a new %q provider needs an api_key reference", entry.Driver)
	}

	if draft.Providers == nil {
		draft.Providers = make(map[string]config.ProviderEntry)
	}

	draft.Providers[o.name] = entry

	return nil
}

type removeProvider struct{ name string }

func (o *removeProvider) Path() string { return "providers." + o.name }

func (o *removeProvider) Summary() string { return "remove provider " + o.name }

func (o *removeProvider) apply(draft *config.UnifiedConfig) error {
	if _, ok := draft.Providers[o.name]; !ok {
		return fmt.Errorf("no provider named %q", o.name)
	}

	if len(draft.Providers) == 1 {
		return errors.New("this is the only provider — add another before removing it")
	}

	if used := modelsOfProvider(draft, o.name); len(used) > 0 {
		return fmt.Errorf(
			"still used by %s — remove those models first",
			strings.Join(used, ", "),
		)
	}

	draft.Providers = maps.Clone(draft.Providers)
	delete(draft.Providers, o.name)

	return nil
}

// checkCredential enforces the ${VAR}-only rule on a credential-bearing field.
// Empty means "unchanged" and is the caller's business, not this check's.
func checkCredential(field, value string) error {
	if value == "" || secretRef.MatchString(value) {
		return nil
	}

	return fmt.Errorf("%s %s", field, refOnlyMessage)
}

func modelsOfProvider(draft *config.UnifiedConfig, provider string) []string {
	var out []string

	for _, m := range draft.Models {
		if m.Provider == provider {
			out = append(out, "model "+m.ID)
		}
	}

	return out
}
