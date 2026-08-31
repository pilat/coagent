package sessionlifecycle

import "sync"

type Registry[T any] interface {
	Load(sessionID int64) (T, bool)
	Use(sessionID int64, fn func(T)) bool
	Register(sessionID int64, value T) (existing T, registered bool)
	Delete(sessionID int64) (T, bool)
	CloseAndSnapshot() []T
	Closed() bool
	Len() int
}

var _ Registry[int] = (*registry[int])(nil)

type registry[T any] struct {
	mu     sync.Mutex
	values map[int64]T
	closed bool
}

func NewRegistry[T any]() Registry[T] { return &registry[T]{values: make(map[int64]T)} }

func (r *registry[T]) Load(sessionID int64) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	value, ok := r.values[sessionID]

	return value, ok
}

func (r *registry[T]) Use(sessionID int64, fn func(T)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	value, ok := r.values[sessionID]
	if !ok {
		return false
	}

	fn(value)

	return true
}

func (r *registry[T]) Register(sessionID int64, value T) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.values[sessionID]; ok {
		return existing, false
	}

	if r.closed {
		var zero T

		return zero, false
	}

	r.values[sessionID] = value

	var zero T

	return zero, true
}

func (r *registry[T]) Delete(sessionID int64) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.values[sessionID]
	if !ok {
		var zero T

		return zero, false
	}

	delete(r.values, sessionID)

	return existing, true
}

func (r *registry[T]) CloseAndSnapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true

	values := make([]T, 0, len(r.values))
	for _, value := range r.values {
		values = append(values, value)
	}

	return values
}

func (r *registry[T]) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closed
}

func (r *registry[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.values)
}
