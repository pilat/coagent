package assistant

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/catalog"
)

func cardModel(card Card, models []Model) string {
	for _, model := range models {
		if model.ID != card.Model {
			continue
		}

		name := cmp.Or(model.DisplayName, model.ID)
		if len(model.EffortLevels) > 0 {
			name += " · " + catalog.ClampEffort(cmp.Or(card.Effort, "medium"), model.EffortAllowlist)
		}

		return name
	}

	return card.Model
}

func cardBudget(card Card, now time.Time) string {
	if card.Budget == nil {
		return ""
	}

	record := card.Budget
	if record.State == budget.Fired {
		state := "fired"
		if record.ParkPhase == "parked" {
			state = "parked"
		}

		return fmt.Sprintf("budget %s (%s)", state, record.FiredReason)
	}

	if record.State != budget.Armed {
		return ""
	}

	treeCost := 0.0
	if card.TreeCostUSD != nil {
		treeCost = *card.TreeCostUSD
	}

	cost, duration := record.Remaining(treeCost, now)

	var parts []string
	if cost != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *cost))
	}

	if duration != nil {
		minutes := int(*duration / time.Minute)

		text := fmt.Sprintf("%dm", minutes)
		if minutes >= 60 {
			text = fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
		}

		parts = append(parts, text)
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " · ") + " left"
}
