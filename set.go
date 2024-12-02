// Please review this code. Iter and Maps are from the go 1.23 standard library.

package main

import (
	"iter"
	"maps"
)

type Set[T comparable] map[T]struct{}

func NewSet[T comparable](ks ...T) *Set[T] {
	set := &Set[T]{}
	for _, k := range ks {
		set.Add(k)
	}
	return set
}

func (set *Set[T]) All() iter.Seq[T] {
	return maps.Keys(*set)
}

func (set *Set[T]) Keys() []T {
	var ks []T
	for k := range *set {
		ks = append(ks, k)
	}
	return ks
}

func (set *Set[T]) Size() int {
	return len(*set)
}

func (set *Set[T]) Add(v T) {
	(*set)[v] = struct{}{}
}

func (set *Set[T]) Has(v T) bool {
	_, has := (*set)[v]
	return has
}

func (set *Set[T]) Del(v T) {
	delete(*set, v)
}

func (set *Set[T]) Union(other *Set[T]) *Set[T] {
	out := NewSet[T]()
	for k := range set.All() {
		out.Add(k)
	}
	for k := range other.All() {
		out.Add(k)
	}
	return out
}

func (set *Set[T]) Intersection(other *Set[T]) *Set[T] {
	out := NewSet[T]()
	for k := range set.All() {
		if other.Has(k) {
			out.Add(k)
		}
	}
	return out
}

func (set *Set[T]) Difference(other *Set[T]) *Set[T] {
	out := NewSet[T]()
	for k := range set.All() {
		if !other.Has(k) {
			out.Add(k)
		}
	}
	return out
}

