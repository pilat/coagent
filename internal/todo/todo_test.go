package todo

import (
	"sync"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Run("returns non-nil service", func(t *testing.T) {
		s := New()
		if s == nil {
			t.Fatal("New() returned nil")
		}
	})

	t.Run("initial count is zero", func(t *testing.T) {
		s := New()
		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})

	t.Run("initial list is empty", func(t *testing.T) {
		s := New()

		items := s.List()
		if len(items) != 0 {
			t.Errorf("List() returned %d items, want 0", len(items))
		}
	})
}

func TestAdd(t *testing.T) {
	t.Run("adds item and returns it", func(t *testing.T) {
		s := New()
		item := s.Add("Test task", PriorityHigh)

		if item == nil {
			t.Fatal("Add() returned nil")
		}
		if item.Content != "Test task" {
			t.Errorf("Content = %q, want %q", item.Content, "Test task")
		}
		if item.Priority != PriorityHigh {
			t.Errorf("Priority = %q, want %q", item.Priority, PriorityHigh)
		}
		if item.Status != StatusPending {
			t.Errorf("Status = %q, want %q", item.Status, StatusPending)
		}
	})

	t.Run("generates unique IDs", func(t *testing.T) {
		s := New()
		item1 := s.Add("Task 1", PriorityMedium)
		item2 := s.Add("Task 2", PriorityMedium)

		if item1.ID == "" || item2.ID == "" {
			t.Error("Add() did not generate IDs")
		}
		if item1.ID == item2.ID {
			t.Errorf("Add() generated duplicate IDs: %s", item1.ID)
		}
	})

	t.Run("sets timestamps", func(t *testing.T) {
		s := New()
		before := time.Now()
		item := s.Add("Task", PriorityLow)
		after := time.Now()

		if item.CreatedAt.IsZero() {
			t.Error("CreatedAt is zero")
		}
		if item.UpdatedAt.IsZero() {
			t.Error("UpdatedAt is zero")
		}
		if item.CreatedAt.Before(before) || item.CreatedAt.After(after) {
			t.Error("CreatedAt is not set correctly")
		}
		if item.UpdatedAt.Before(before) || item.UpdatedAt.After(after) {
			t.Error("UpdatedAt is not set correctly")
		}
	})

	t.Run("increments count", func(t *testing.T) {
		s := New()
		s.Add("Task 1", PriorityMedium)
		s.Add("Task 2", PriorityMedium)

		if count := s.Count(); count != 2 {
			t.Errorf("Count() = %d, want 2", count)
		}
	})
}

func TestGet(t *testing.T) {
	t.Run("returns existing item", func(t *testing.T) {
		s := New()
		added := s.Add("Task", PriorityMedium)

		got := s.Get(added.ID)
		if got == nil {
			t.Fatal("Get() returned nil for existing item")
		}
		if got.ID != added.ID {
			t.Errorf("Get() returned item with wrong ID: %s", got.ID)
		}
		if got.Content != added.Content {
			t.Errorf("Get() returned item with wrong Content: %s", got.Content)
		}
	})

	t.Run("returns nil for non-existent ID", func(t *testing.T) {
		s := New()
		got := s.Get("non-existent-id")
		if got != nil {
			t.Errorf("Get() = %v, want nil", got)
		}
	})

	t.Run("returns nil for empty ID", func(t *testing.T) {
		s := New()
		got := s.Get("")
		if got != nil {
			t.Errorf("Get() = %v, want nil", got)
		}
	})
}

func TestUpdate(t *testing.T) {
	t.Run("updates all fields", func(t *testing.T) {
		s := New()
		item := s.Add("Original", PriorityLow)
		originalUpdatedAt := item.UpdatedAt

		time.Sleep(10 * time.Millisecond) // Ensure time difference
		ok := s.Update(item.ID, "Updated", StatusInProgress, PriorityHigh)

		if !ok {
			t.Error("Update() returned false for existing item")
		}

		updated := s.Get(item.ID)
		if updated.Content != "Updated" {
			t.Errorf("Content = %q, want %q", updated.Content, "Updated")
		}
		if updated.Status != StatusInProgress {
			t.Errorf("Status = %q, want %q", updated.Status, StatusInProgress)
		}
		if updated.Priority != PriorityHigh {
			t.Errorf("Priority = %q, want %q", updated.Priority, PriorityHigh)
		}
		if !updated.UpdatedAt.After(originalUpdatedAt) {
			t.Error("UpdatedAt was not updated")
		}
	})

	t.Run("updates only content", func(t *testing.T) {
		s := New()
		item := s.Add("Original", PriorityMedium)
		originalStatus := item.Status
		originalPriority := item.Priority

		ok := s.Update(item.ID, "Updated", "", "")
		if !ok {
			t.Error("Update() returned false")
		}

		updated := s.Get(item.ID)
		if updated.Content != "Updated" {
			t.Errorf("Content = %q, want %q", updated.Content, "Updated")
		}
		if updated.Status != originalStatus {
			t.Errorf("Status changed unexpectedly to %q", updated.Status)
		}
		if updated.Priority != originalPriority {
			t.Errorf("Priority changed unexpectedly to %q", updated.Priority)
		}
	})

	t.Run("updates only status", func(t *testing.T) {
		s := New()
		item := s.Add("Task", PriorityMedium)
		originalContent := item.Content
		originalPriority := item.Priority

		ok := s.Update(item.ID, "", StatusCompleted, "")
		if !ok {
			t.Error("Update() returned false")
		}

		updated := s.Get(item.ID)
		if updated.Content != originalContent {
			t.Errorf("Content changed unexpectedly to %q", updated.Content)
		}
		if updated.Status != StatusCompleted {
			t.Errorf("Status = %q, want %q", updated.Status, StatusCompleted)
		}
		if updated.Priority != originalPriority {
			t.Errorf("Priority changed unexpectedly to %q", updated.Priority)
		}
	})

	t.Run("updates only priority", func(t *testing.T) {
		s := New()
		item := s.Add("Task", PriorityLow)
		originalContent := item.Content
		originalStatus := item.Status

		ok := s.Update(item.ID, "", "", PriorityHigh)
		if !ok {
			t.Error("Update() returned false")
		}

		updated := s.Get(item.ID)
		if updated.Content != originalContent {
			t.Errorf("Content changed unexpectedly to %q", updated.Content)
		}
		if updated.Status != originalStatus {
			t.Errorf("Status changed unexpectedly to %q", updated.Status)
		}
		if updated.Priority != PriorityHigh {
			t.Errorf("Priority = %q, want %q", updated.Priority, PriorityHigh)
		}
	})

	t.Run("returns false for non-existent ID", func(t *testing.T) {
		s := New()
		ok := s.Update("non-existent", "Content", StatusInProgress, PriorityHigh)
		if ok {
			t.Error("Update() returned true for non-existent ID")
		}
	})

	t.Run("empty values do not update", func(t *testing.T) {
		s := New()
		item := s.Add("Task", PriorityMedium)
		item.Status = StatusInProgress

		ok := s.Update(item.ID, "", "", "")
		if !ok {
			t.Error("Update() returned false")
		}

		updated := s.Get(item.ID)
		if updated.Content != "Task" {
			t.Errorf("Content changed unexpectedly")
		}
		if updated.Status != StatusInProgress {
			t.Errorf("Status changed unexpectedly")
		}
		if updated.Priority != PriorityMedium {
			t.Errorf("Priority changed unexpectedly")
		}
	})
}

func TestDelete(t *testing.T) {
	t.Run("deletes existing item", func(t *testing.T) {
		s := New()
		item := s.Add("Task", PriorityMedium)

		ok := s.Delete(item.ID)
		if !ok {
			t.Error("Delete() returned false for existing item")
		}

		if s.Get(item.ID) != nil {
			t.Error("Item still exists after Delete()")
		}
		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})

	t.Run("returns false for non-existent ID", func(t *testing.T) {
		s := New()
		ok := s.Delete("non-existent")
		if ok {
			t.Error("Delete() returned true for non-existent ID")
		}
	})

	t.Run("returns false for empty ID", func(t *testing.T) {
		s := New()

		ok := s.Delete("")
		if ok {
			t.Error("Delete() returned true for empty ID")
		}
	})
}

func TestList(t *testing.T) {
	t.Run("returns empty slice when no items", func(t *testing.T) {
		s := New()

		items := s.List()
		if items == nil {
			t.Error("List() returned nil instead of empty slice")
		}
		if len(items) != 0 {
			t.Errorf("List() returned %d items, want 0", len(items))
		}
	})

	t.Run("returns all items", func(t *testing.T) {
		s := New()
		item1 := s.Add("Task 1", PriorityHigh)
		item2 := s.Add("Task 2", PriorityMedium)
		item3 := s.Add("Task 3", PriorityLow)

		items := s.List()
		if len(items) != 3 {
			t.Errorf("List() returned %d items, want 3", len(items))
		}

		// Check all items are present
		ids := make(map[string]bool)
		for _, item := range items {
			ids[item.ID] = true
		}
		if !ids[item1.ID] || !ids[item2.ID] || !ids[item3.ID] {
			t.Error("List() missing some items")
		}
	})

	t.Run("returns independent copy", func(t *testing.T) {
		s := New()
		s.Add("Task", PriorityMedium)

		items1 := s.List()
		items2 := s.List()

		// Should be different slices
		if &items1[0] == &items2[0] {
			t.Error("List() returned same slice reference")
		}
	})
}

func TestReplace(t *testing.T) {
	t.Run("replaces all items", func(t *testing.T) {
		s := New()
		s.Add("Old 1", PriorityMedium)
		s.Add("Old 2", PriorityMedium)

		newItems := []*Item{
			{ID: "id1", Content: "New 1", Status: StatusPending, Priority: PriorityHigh},
			{ID: "id2", Content: "New 2", Status: StatusInProgress, Priority: PriorityMedium},
		}

		s.Replace(newItems)

		if count := s.Count(); count != 2 {
			t.Errorf("Count() = %d, want 2", count)
		}

		items := s.List()

		foundIDs := make(map[string]bool)
		for _, item := range items {
			foundIDs[item.ID] = true
		}
		if !foundIDs["id1"] || !foundIDs["id2"] {
			t.Error("Replace() did not set correct IDs")
		}
	})

	t.Run("generates IDs for items without ID", func(t *testing.T) {
		s := New()
		newItems := []*Item{
			{Content: "New 1", Status: StatusPending, Priority: PriorityHigh},
		}

		s.Replace(newItems)

		items := s.List()
		if len(items) != 1 {
			t.Fatalf("List() returned %d items, want 1", len(items))
		}
		if items[0].ID == "" {
			t.Error("Replace() did not generate ID for item without ID")
		}
	})

	t.Run("sets defaults for empty fields", func(t *testing.T) {
		s := New()
		newItems := []*Item{
			{ID: "test-id", Content: "Task"}, // No Status or Priority
		}

		s.Replace(newItems)

		item := s.Get("test-id")
		if item.Status != StatusPending {
			t.Errorf("Status = %q, want %q", item.Status, StatusPending)
		}
		if item.Priority != PriorityMedium {
			t.Errorf("Priority = %q, want %q", item.Priority, PriorityMedium)
		}
	})

	t.Run("sets timestamps for items without them", func(t *testing.T) {
		s := New()
		newItems := []*Item{
			{ID: "test-id", Content: "Task", Status: StatusPending, Priority: PriorityMedium},
		}

		s.Replace(newItems)

		item := s.Get("test-id")
		if item.CreatedAt.IsZero() {
			t.Error("CreatedAt is zero")
		}
		if item.UpdatedAt.IsZero() {
			t.Error("UpdatedAt is zero")
		}
	})

	t.Run("preserves existing CreatedAt", func(t *testing.T) {
		s := New()
		existingTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
		newItems := []*Item{
			{ID: "test-id", Content: "Task", Status: StatusPending, Priority: PriorityMedium, CreatedAt: existingTime},
		}

		s.Replace(newItems)

		item := s.Get("test-id")
		if !item.CreatedAt.Equal(existingTime) {
			t.Error("CreatedAt was not preserved")
		}
	})

	t.Run("clears existing items when given empty list", func(t *testing.T) {
		s := New()
		s.Add("Task 1", PriorityMedium)
		s.Add("Task 2", PriorityMedium)

		s.Replace([]*Item{})

		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})

	t.Run("clears existing items when given nil", func(t *testing.T) {
		s := New()
		s.Add("Task", PriorityMedium)

		s.Replace(nil)

		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})
}

func TestClear(t *testing.T) {
	t.Run("removes all items", func(t *testing.T) {
		s := New()
		s.Add("Task 1", PriorityHigh)
		s.Add("Task 2", PriorityMedium)
		s.Add("Task 3", PriorityLow)

		s.Clear()

		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
		if items := s.List(); len(items) != 0 {
			t.Errorf("List() returned %d items, want 0", len(items))
		}
	})

	t.Run("works on empty service", func(t *testing.T) {
		s := New()
		s.Clear() // Should not panic
		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})
}

func TestCount(t *testing.T) {
	t.Run("returns zero for new service", func(t *testing.T) {
		s := New()
		if count := s.Count(); count != 0 {
			t.Errorf("Count() = %d, want 0", count)
		}
	})

	t.Run("returns correct count after adds", func(t *testing.T) {
		s := New()
		s.Add("Task 1", PriorityMedium)
		s.Add("Task 2", PriorityMedium)
		s.Add("Task 3", PriorityMedium)

		if count := s.Count(); count != 3 {
			t.Errorf("Count() = %d, want 3", count)
		}
	})

	t.Run("returns correct count after deletes", func(t *testing.T) {
		s := New()
		item1 := s.Add("Task 1", PriorityMedium)
		item2 := s.Add("Task 2", PriorityMedium)
		s.Add("Task 3", PriorityMedium)

		s.Delete(item1.ID)
		s.Delete(item2.ID)

		if count := s.Count(); count != 1 {
			t.Errorf("Count() = %d, want 1", count)
		}
	})
}

func TestConcurrent(t *testing.T) {
	t.Run("concurrent adds", func(t *testing.T) {
		s := New()
		const numGoroutines = 100

		const addsPerGoroutine = 10

		var wg sync.WaitGroup
		for i := range numGoroutines {
			wg.Add(1)

			go func(_ int) {
				defer wg.Done()
				for range addsPerGoroutine {
					s.Add("Task", PriorityMedium)
				}
			}(i)
		}
		wg.Wait()

		expected := numGoroutines * addsPerGoroutine
		if count := s.Count(); count != expected {
			t.Errorf("Count() = %d, want %d", count, expected)
		}
	})

	t.Run("concurrent reads and writes", func(t *testing.T) {
		s := New()
		// Pre-populate with some items
		for range 10 {
			s.Add("Task", PriorityMedium)
		}

		var wg sync.WaitGroup
		done := make(chan bool)

		// Writers
		for range 5 {
			wg.Go(func() {
				for range 20 {
					s.Add("New Task", PriorityHigh)
					time.Sleep(time.Microsecond)
				}
			})
		}

		// Readers
		for range 5 {
			wg.Go(func() {
				for range 20 {
					_ = s.List()
					_ = s.Count()

					time.Sleep(time.Microsecond)
				}
			})
		}

		// Modifiers
		for range 5 {
			wg.Go(func() {
				for range 10 {
					items := s.List()
					if len(items) > 0 {
						s.Update(items[0].ID, "Updated", "", "")
					}
					time.Sleep(time.Microsecond * 2)
				}
			})
		}

		// Deleters
		for range 3 {
			wg.Go(func() {
				for range 10 {
					items := s.List()
					if len(items) > 0 {
						s.Delete(items[len(items)-1].ID)
					}
					time.Sleep(time.Microsecond * 3)
				}
			})
		}

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("Concurrent test timed out")
		}
	})

	t.Run("concurrent Replace operations", func(t *testing.T) {
		s := New()

		var wg sync.WaitGroup

		for i := range 10 {
			wg.Add(1)

			go func(_ int) {
				defer wg.Done()
				items := []*Item{
					{ID: "id1", Content: "Task 1", Status: StatusPending, Priority: PriorityHigh},
					{ID: "id2", Content: "Task 2", Status: StatusInProgress, Priority: PriorityMedium},
				}
				s.Replace(items)
			}(i)
		}

		wg.Wait()

		// Service should be in a consistent state
		count := s.Count()
		if count != 0 && count != 2 {
			t.Errorf("Count() = %d, want 0 or 2 after concurrent Replace", count)
		}
	})

	t.Run("concurrent Clear and Add", func(t *testing.T) {
		s := New()

		var wg sync.WaitGroup

		for i := range 20 {
			wg.Add(1)

			go func(n int) {
				defer wg.Done()
				if n%2 == 0 {
					s.Add("Task", PriorityMedium)
				} else {
					s.Clear()
				}
			}(i)
		}

		wg.Wait()

		// Service should not panic and should be in a valid state
		_ = s.Count()
		_ = s.List()
	})
}
