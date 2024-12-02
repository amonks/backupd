package main

import "iter"

type OrderedMap[T any] struct {
	ks []string
	vs map[string]T
}

func NewOrderedMap[T any]() *OrderedMap[T] {
	return &OrderedMap[T]{
		vs: make(map[string]T),
	}
}

func (om *OrderedMap[T]) Append(k string, v T) {
	om.ks = append(om.ks, k)
	om.vs[k] = v
}

func (om *OrderedMap[T]) Has(k string) bool {
	_, has := om.vs[k]
	return has
}

func (om *OrderedMap[T]) Get(k string) T {
	t, _ := om.vs[k]
	return t
}

func (om *OrderedMap[T]) All() iter.Seq2[string, T] {
	return func(yield func(string, T) bool) {
		for _, k := range om.ks {
			if !yield(k, om.vs[k]) {
				return
			}
		}
	}
}
