package sessionlifecycle

import "sync"

type Queue[T any] interface {
	Push(value T)
	PushUnique(value T, same func(T, T) bool) bool
	PopFirst(predicate func(T) bool) (T, bool)
	Remove(predicate func(T) bool)
	Len() int
}

var _ Queue[int] = (*queue[int])(nil)

type queue[T any] struct {
	mu     sync.Mutex
	values []T
}

func NewQueue[T any]() Queue[T] { return &queue[T]{} }

func (q *queue[T]) Push(value T) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.values = append(q.values, value)
}

func (q *queue[T]) PushUnique(value T, same func(T, T) bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, existing := range q.values {
		if same(existing, value) {
			return false
		}
	}

	q.values = append(q.values, value)

	return true
}

func (q *queue[T]) PopFirst(predicate func(T) bool) (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, value := range q.values {
		if !predicate(value) {
			continue
		}

		q.values = append(q.values[:i], q.values[i+1:]...)

		return value, true
	}

	var zero T

	return zero, false
}

func (q *queue[T]) Remove(predicate func(T) bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	kept := q.values[:0]
	for _, value := range q.values {
		if !predicate(value) {
			kept = append(kept, value)
		}
	}

	q.values = kept
}

func (q *queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.values)
}
