package id

import (
	"regexp"
	"sync"
	"testing"
)

func TestGenerate_Basic(t *testing.T) {
	t.Run("returns non-empty ID", func(t *testing.T) {
		id := Generate()
		if id == "" {
			t.Error("Generate() returned empty string")
		}
	})

	t.Run("returns correct length", func(t *testing.T) {
		id := Generate()
		// 8 random bytes → 16 hex characters
		expectedLen := 16
		if len(id) != expectedLen {
			t.Errorf("Generate() returned ID of length %d, want %d", len(id), expectedLen)
		}
	})

	t.Run("multiple calls return valid IDs", func(t *testing.T) {
		for i := range 10 {
			id := Generate()
			if len(id) != 16 {
				t.Errorf("Iteration %d: Generate() returned ID of length %d, want 16", i, len(id))
			}
		}
	})
}

func TestGenerate_Uniqueness(t *testing.T) {
	t.Run("sequential calls produce unique IDs", func(t *testing.T) {
		const count = 1000
		seen := make(map[string]bool, count)

		for i := range count {
			id := Generate()
			if seen[id] {
				t.Fatalf("Generate() produced duplicate ID: %s at iteration %d", id, i)
			}
			seen[id] = true
		}
	})
}

func TestGenerate_Format(t *testing.T) {
	// Hex string: lowercase a-f and digits 0-9
	hexRegex := regexp.MustCompile(`^[0-9a-f]{16}$`)

	t.Run("ID contains only valid hex characters", func(t *testing.T) {
		for range 100 {
			id := Generate()
			if !hexRegex.MatchString(id) {
				t.Errorf("Generate() returned ID with invalid characters: %s", id)
			}
		}
	})
}

func TestGenerate_Concurrent(t *testing.T) {
	t.Run("concurrent calls produce unique IDs", func(t *testing.T) {
		const numGoroutines = 100
		const idsPerGoroutine = 100

		var wg sync.WaitGroup
		idChan := make(chan string, numGoroutines*idsPerGoroutine)

		for range numGoroutines {
			wg.Go(func() {
				for range idsPerGoroutine {
					idChan <- Generate()
				}
			})
		}

		go func() {
			wg.Wait()
			close(idChan)
		}()

		seen := make(map[string]bool, numGoroutines*idsPerGoroutine)
		count := 0
		for id := range idChan {
			count++
			if seen[id] {
				t.Fatalf("Concurrent Generate() produced duplicate ID: %s", id)
			}
			seen[id] = true
		}

		expectedCount := numGoroutines * idsPerGoroutine
		if count != expectedCount {
			t.Errorf("Expected %d IDs, got %d", expectedCount, count)
		}
	})
}
