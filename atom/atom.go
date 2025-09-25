package atom

import "sync"

type Atom[T any] struct {
	mu sync.RWMutex
	v  T
}

func New[T any](v T) *Atom[T] {
	return &Atom[T]{v: v}
}

func (at *Atom[T]) Deref() T {
	at.mu.RLock()
	defer at.mu.RUnlock()

	return at.v
}

func (at *Atom[T]) Swap(fn func(T) T) {
	at.mu.Lock()
	defer at.mu.Unlock()

	at.v = fn(at.v)
}

func (at *Atom[T]) Reset(v T) {
	at.mu.Lock()
	defer at.mu.Unlock()

	at.v = v
}
