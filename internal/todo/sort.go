package todo

import "sort"

// priorityRank orders high → medium → low; unknown values sort after low.
func priorityRank(p Priority) int {
	switch p {
	case PriorityHigh:
		return 0
	case PriorityMedium:
		return 1
	case PriorityLow:
		return 2
	default:
		return 3
	}
}

// SortCanonical orders items in place by priority rank, then creation time
// ascending. Equal priority and creation time fall back to ascending ID, so
// repeated reads and durable projections stay deterministic. Legacy items with
// a zero CreatedAt sort before same-rank items that carry a timestamp; both
// live and projected paths share this behavior.
func SortCanonical(items []*Item) {
	sort.Slice(items, func(i, j int) bool {
		ri, rj := priorityRank(items[i].Priority), priorityRank(items[j].Priority)
		if ri != rj {
			return ri < rj
		}

		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}

		return items[i].ID < items[j].ID
	})
}
