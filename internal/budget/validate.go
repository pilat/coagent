//nolint:wrapcheck // Duration parser errors are returned as validation details.; nosemgrep: semgrep.coagent-no-preamble-before-package
package budget

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/tool"
)

const (
	minDuration = time.Minute
	maxDuration = 365 * 24 * time.Hour
	maxCostUSD  = 1_000_000
)

func normalizeCost(value *float64) (*float64, error) {
	if value == nil {
		return nil, nil //nolint:nilnil // Omitted optional limit is valid.
	}

	if *value <= 0 || *value > maxCostUSD || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return nil, errors.New("cost_usd must be finite, positive, and no more than 1000000")
	}

	normalized := math.Round(*value*1_000_000) / 1_000_000

	return &normalized, nil
}

func parseRelativeDuration(value string) (*time.Duration, error) {
	if value == "" {
		return nil, nil //nolint:nilnil // Omitted optional limit is valid.
	}

	if strings.Contains(value, "T") {
		return nil, errors.New("duration must be relative, not an RFC3339 timestamp")
	}

	duration, err := tool.ParseDuration(value)
	if err != nil {
		return nil, err
	}

	if duration < minDuration || duration > maxDuration {
		return nil, errors.New("duration must be between 1 minute and 365 days")
	}

	return &duration, nil
}
