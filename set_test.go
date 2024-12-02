package main

import (
	"testing"
)

func TestSet(t *testing.T) {
	s := NewSet[string]("a", "b", "c")
	s.Add("d")
	if !s.Has("a") {
		t.Errorf(`s.Has("a")=false; expect: true`)
	}
	if s.Size() != 4 {
		t.Errorf(`s.Size()=%d; expect: 4`, s.Size())
	}

	n := 0
	for range s.All() {
		n++
	}
	if n != 4 {
		t.Errorf(`s.All() called %d times; expect: 4`, n)
	}
}
