package todo

import (
	"sync"
	"time"

	"github.com/pilat/coagent/internal/id"
)

// Service manages an isolated, concurrency-safe collection of todo items.
type Service struct {
	mu    sync.RWMutex
	items map[string]*Item
}

func New() *Service {
	return &Service{
		items: make(map[string]*Item),
	}
}

func (s *Service) Add(content string, priority Priority) *Item {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	item := &Item{
		ID:        id.Generate(),
		Content:   content,
		Status:    StatusPending,
		Priority:  priority,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.items[item.ID] = item

	return item
}

func (s *Service) Get(itemID string) *Item {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.items[itemID]
	if !ok {
		return nil
	}
	// Return a copy to prevent external modification
	cpy := *item

	return &cpy
}

func (s *Service) Update(itemID, content string, status Status, priority Priority) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	item, ok := s.items[itemID]
	if !ok {
		return false
	}

	if content != "" {
		item.Content = content
	}

	if status != "" {
		item.Status = status
	}

	if priority != "" {
		item.Priority = priority
	}

	item.UpdatedAt = time.Now()

	return true
}

func (s *Service) Delete(itemID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.items[itemID]; !ok {
		return false
	}

	delete(s.items, itemID)

	return true
}

func (s *Service) List() []*Item {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]*Item, 0, len(s.items))
	for _, item := range s.items {
		// Return copies to prevent external modification
		cpy := *item
		items = append(items, &cpy)
	}

	return items
}

func (s *Service) Replace(items []*Item) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items = make(map[string]*Item)
	now := time.Now()

	for _, item := range items {
		// Create a copy to avoid modifying the original
		cpy := *item
		if cpy.ID == "" {
			cpy.ID = id.Generate()
		}

		if cpy.Status == "" {
			cpy.Status = StatusPending
		}

		if cpy.Priority == "" {
			cpy.Priority = PriorityMedium
		}

		if cpy.CreatedAt.IsZero() {
			cpy.CreatedAt = now
		}

		cpy.UpdatedAt = now
		s.items[cpy.ID] = &cpy
	}
}

func (s *Service) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items = make(map[string]*Item)
}

func (s *Service) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.items)
}
