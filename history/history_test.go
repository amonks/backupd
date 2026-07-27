package history

import (
	"fmt"
	"testing"
	"time"
)

func TestCyclesNewestFirstAndCapped(t *testing.T) {
	h := New()
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	for i := range keep + 10 {
		h.RecordCycle(Cycle{
			StartedAt: base.Add(time.Duration(i) * time.Hour),
			StoppedAt: base.Add(time.Duration(i)*time.Hour + time.Minute),
			OK:        true,
			Error:     fmt.Sprintf("cycle-%d", i),
		})
	}
	cycles := h.Cycles()
	if len(cycles) != keep {
		t.Fatalf("expected %d cycles, got %d", keep, len(cycles))
	}
	if cycles[0].Error != fmt.Sprintf("cycle-%d", keep+9) {
		t.Errorf("expected newest cycle first, got %s", cycles[0].Error)
	}
	if cycles[keep-1].Error != "cycle-10" {
		t.Errorf("expected oldest retained cycle to be cycle-10, got %s", cycles[keep-1].Error)
	}
}

func TestLastSuccess(t *testing.T) {
	h := New()
	if _, ok := h.LastSuccess("/foo"); ok {
		t.Error("expected no last success initially")
	}
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	h.RecordDatasetSuccess("/foo", at)
	got, ok := h.LastSuccess("/foo")
	if !ok || !got.Equal(at) {
		t.Errorf("expected last success %s, got %s (ok=%v)", at, got, ok)
	}
	if _, ok := h.LastSuccess("/bar"); ok {
		t.Error("expected no last success for other dataset")
	}
}
