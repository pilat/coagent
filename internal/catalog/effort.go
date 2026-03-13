package catalog

import "slices"

// effortOrder is the effort vocabulary both catalogs draw from, weakest first.
// models.dev emits ascending, OpenRouter descending — everything is normalized here.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// SortEfforts normalizes a catalog's level list to canonical weakest-first order,
// dropping duplicates and anything outside the known vocabulary.
func SortEfforts(levels []string) []string {
	if len(levels) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(levels))

	out := make([]string, 0, len(levels))
	for _, want := range effortOrder {
		if !slices.Contains(levels, want) {
			continue
		}

		if _, dup := seen[want]; dup {
			continue
		}

		seen[want] = struct{}{}

		out = append(out, want)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ClampEffort maps a requested level onto the nearest one a model accepts. An empty
// allowlist passes the level through — there is nothing to clamp against. Ties go
// to the weaker level, so a clamp never silently buys more tokens than asked for.
func ClampEffort(level string, allowed []string) string {
	if len(allowed) == 0 || slices.Contains(allowed, level) {
		return level
	}

	want := effortRank(level)
	if want < 0 {
		return level
	}

	best, bestDelta := "", len(effortOrder)+1

	for _, candidate := range allowed {
		rank := effortRank(candidate)
		if rank < 0 {
			continue
		}

		delta := rank - want
		if delta < 0 {
			delta = -delta
		}

		if delta < bestDelta || (delta == bestDelta && rank < effortRank(best)) {
			best, bestDelta = candidate, delta
		}
	}

	if best == "" {
		return level
	}

	return best
}

func effortRank(level string) int {
	return slices.Index(effortOrder, level)
}
