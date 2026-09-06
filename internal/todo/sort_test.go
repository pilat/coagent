package todo

import (
	"testing"
	"time"
)

func TestSortCanonical(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	t.Run("orders by priority then creation time", func(t *testing.T) {
		items := []*Item{
			{ID: "low", Priority: PriorityLow, CreatedAt: base},
			{ID: "unknown", Priority: Priority("weird"), CreatedAt: base},
			{ID: "high-late", Priority: PriorityHigh, CreatedAt: base.Add(time.Minute)},
			{ID: "high-early", Priority: PriorityHigh, CreatedAt: base},
			{ID: "medium", Priority: PriorityMedium, CreatedAt: base},
			{ID: "empty", Priority: "", CreatedAt: base},
		}

		SortCanonical(items)

		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, item.ID)
		}

		want := []string{"high-early", "high-late", "medium", "low", "empty", "unknown"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("zero timestamps sort before same-rank timestamps", func(t *testing.T) {
		items := []*Item{
			{ID: "later", Priority: PriorityHigh, CreatedAt: base},
			{ID: "zero", Priority: PriorityHigh},
		}

		SortCanonical(items)

		if items[0].ID != "zero" || items[1].ID != "later" {
			t.Fatalf("got %s, %s; want zero first", items[0].ID, items[1].ID)
		}
	})

	t.Run("exact priority and time ties resolve by ascending ID", func(t *testing.T) {
		items := []*Item{
			{ID: "b", Priority: PriorityMedium, CreatedAt: base},
			{ID: "a", Priority: PriorityMedium, CreatedAt: base},
			{ID: "c", Priority: PriorityMedium, CreatedAt: base},
		}

		SortCanonical(items)

		if items[0].ID != "a" || items[1].ID != "b" || items[2].ID != "c" {
			t.Fatalf("tie-break by ID failed: %v", items)
		}
	})

	t.Run("sorts in place without changing items", func(t *testing.T) {
		items := []*Item{
			{ID: "b", Priority: PriorityLow, CreatedAt: base},
			{ID: "a", Priority: PriorityLow, CreatedAt: base},
		}

		SortCanonical(items)

		if items[0].ID != "a" || items[1].ID != "b" {
			t.Fatalf("unexpected order: %v", items)
		}
		if items[0].Content != "" || items[0].Status != "" {
			t.Fatalf("sorter must not mutate item fields")
		}
	})
}
