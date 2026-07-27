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

func TestLastFailure(t *testing.T) {
	h := New()
	if _, ok := h.LastFailure("/foo"); ok {
		t.Error("expected no last failure initially")
	}
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	h.RecordDatasetFailure("/foo", at, "out of space")
	got, ok := h.LastFailure("/foo")
	if !ok || !got.At.Equal(at) || got.Error != "out of space" {
		t.Errorf("expected failure at %s, got %+v (ok=%v)", at, got, ok)
	}

	// A later success does not erase the failure record: the dashboard
	// compares timestamps to decide which is current.
	h.RecordDatasetSuccess("/foo", at.Add(time.Hour))
	if _, ok := h.LastFailure("/foo"); !ok {
		t.Error("expected failure record to survive a later success")
	}
	if _, ok := h.LastFailure("/bar"); ok {
		t.Error("expected no failure for other dataset")
	}
}

func TestOpsNewestFirstAndCapped(t *testing.T) {
	h := New()
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	for i := range keepOps + 50 {
		h.RecordOp(Op{
			At:        base.Add(time.Duration(i) * time.Second),
			Dataset:   "/foo",
			Operation: fmt.Sprintf("op-%d", i),
			Duration:  time.Second,
		})
	}
	ops := h.Ops()
	if len(ops) != keepOps {
		t.Fatalf("expected %d ops, got %d", keepOps, len(ops))
	}
	if ops[0].Operation != fmt.Sprintf("op-%d", keepOps+49) {
		t.Errorf("expected newest op first, got %s", ops[0].Operation)
	}
}

func TestLastSnitch(t *testing.T) {
	h := New()
	if _, ok := h.LastSnitch(); ok {
		t.Error("expected no snitch ping initially")
	}
	at := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	h.RecordSnitch(at)
	got, ok := h.LastSnitch()
	if !ok || !got.Equal(at) {
		t.Errorf("expected snitch at %s, got %s (ok=%v)", at, got, ok)
	}
}
