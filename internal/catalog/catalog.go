package catalog

import (
	"maps"
	"regexp"
	"slices"

	"github.com/pilat/coagent/internal/config"
)

// dateSuffix matches the release-date suffix that catalogs and configs disagree
// on: models.dev google-vertex keys carry it, plain anthropic ids may or may not.
var dateSuffix = regexp.MustCompile(`[-@]\d{8}$`)

type (
	// ModelSpec is one model's catalog-resolved metadata. Source names the
	// catalog section it came from, which is what the enrichment log reports.
	ModelSpec struct {
		Name          string
		Source        string
		ContextWindow int
		MaxTokens     int
		Pricing       *config.ModelPricing
		Reasoning     *config.ReasoningSpec
		// Shadowed names the sections that carry this id and lost the Flatten,
		// so enrichment can tell the operator the metadata may be another host's.
		Shadowed []string
	}
)

// Flatten merges every section into one id-keyed map, sections visited in sorted
// order so a duplicated id always resolves to the same section across restarts.
func Flatten(sections map[string]map[string]ModelSpec) map[string]ModelSpec {
	merged := make(map[string]ModelSpec)

	for _, name := range slices.Sorted(maps.Keys(sections)) {
		for id, spec := range sections[name] {
			won, taken := merged[id]
			if !taken {
				merged[id] = spec

				continue
			}

			// Copy rather than append in place: the winner's slice may alias the
			// section's own spec, which Flatten must leave untouched.
			won.Shadowed = append(slices.Clone(won.Shadowed), name)
			merged[id] = won
		}
	}

	return merged
}

// Lookup resolves an id: exact first, then with the date suffix stripped from both
// sides. Ties resolve in sorted key order so restarts agree.
func Lookup(models map[string]ModelSpec, id string) (ModelSpec, bool) {
	if spec, ok := models[id]; ok {
		return spec, true
	}

	normalized := normalizeID(id)

	for _, key := range slices.Sorted(maps.Keys(models)) {
		if normalizeID(key) == normalized {
			return models[key], true
		}
	}

	return ModelSpec{}, false
}

func normalizeID(id string) string {
	return dateSuffix.ReplaceAllString(id, "")
}
