package logger

import (
	"fmt"
	"sync"
	"testing"
)

// TestCap: the buffer is a ring — writes beyond the cap drop the oldest
// entries, so a long-running daemon's memory stays flat.
func TestCap(t *testing.T) {
	l := New("test")
	for i := range keep + 10 {
		l.Printf("entry %d", i)
	}
	logs := l.GetLogs()
	if len(logs) != keep {
		t.Fatalf("got %d entries, want %d", len(logs), keep)
	}
	if want := "entry 10"; logs[0].Log != want {
		t.Errorf("oldest retained entry = %q, want %q", logs[0].Log, want)
	}
	if want := fmt.Sprintf("entry %d", keep+9); logs[len(logs)-1].Log != want {
		t.Errorf("newest entry = %q, want %q", logs[len(logs)-1].Log, want)
	}
}

// TestOrder: entries come back in write order.
func TestOrder(t *testing.T) {
	l := New("test")
	for i := range 10 {
		l.Printf("entry %d", i)
	}
	logs := l.GetLogs()
	if len(logs) != 10 {
		t.Fatalf("got %d entries, want 10", len(logs))
	}
	for i, e := range logs {
		if want := fmt.Sprintf("entry %d", i); e.Log != want {
			t.Errorf("logs[%d] = %q, want %q", i, e.Log, want)
		}
	}
}

// TestConcurrent: writers and readers race under the race detector. The
// sync loop writes while HTTP renders read; both must be safe.
func TestConcurrent(t *testing.T) {
	l := New("test")
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 200 {
				l.Printf("writer %d entry %d", w, i)
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 200 {
				for _, e := range l.GetLogs() {
					_ = e.Log
				}
			}
		})
	}
	wg.Wait()
}
