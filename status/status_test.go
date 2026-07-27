package status

import (
	"testing"
	"time"
)

func at(tr *Tracker, t time.Time) *Tracker {
	tr.now = func() time.Time { return t }
	return tr
}

func TestPhaseTransitions(t *testing.T) {
	notifies := 0
	tr := New(func() { notifies++ })
	t0 := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if got := tr.Get(); got.Phase != Starting {
		t.Fatalf("expected Starting, got %s", got.Phase)
	}

	at(tr, t0).StartCycle()
	a := tr.Get()
	if a.Phase != Refreshing || !a.CycleStarted.Equal(t0) {
		t.Fatalf("expected Refreshing at t0, got %+v", a)
	}

	at(tr, t0.Add(time.Second)).StartDataset("/foo")
	a = tr.Get()
	if a.Phase != Syncing || a.Dataset != "/foo" {
		t.Fatalf("expected Syncing /foo, got %+v", a)
	}
	if !tr.IsSyncing("/foo") || tr.IsSyncing("/bar") {
		t.Fatal("IsSyncing should report only the active dataset")
	}

	tr.StartStep(2, 5, "transfer x")
	a = tr.Get()
	if a.Step != 2 || a.Steps != 5 || a.Operation != "transfer x" {
		t.Fatalf("expected step 2/5, got %+v", a)
	}

	deadline := t0.Add(time.Hour)
	at(tr, t0.Add(2*time.Second)).Wait(deadline, 0)
	a = tr.Get()
	if a.Phase != Idle || !a.Until.Equal(deadline) || a.ConsecutiveFailures != 0 {
		t.Fatalf("expected Idle until deadline, got %+v", a)
	}
	if tr.IsSyncing("/foo") {
		t.Fatal("nothing is syncing while idle")
	}
	if !a.CycleStarted.Equal(t0) {
		t.Fatalf("Wait should preserve CycleStarted, got %+v", a)
	}

	tr.Wait(deadline, 3)
	a = tr.Get()
	if a.Phase != BackingOff || a.ConsecutiveFailures != 3 {
		t.Fatalf("expected BackingOff with 3 failures, got %+v", a)
	}

	if notifies == 0 {
		t.Fatal("expected transitions to notify")
	}
}

func TestProgress(t *testing.T) {
	notifies := 0
	tr := New(func() { notifies++ })
	t0 := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	at(tr, t0).StartDataset("/foo")
	tr.StartStep(1, 1, "transfer")
	base := notifies

	// The first progress report may notify; subsequent ones within the
	// throttle window must not.
	at(tr, t0.Add(10*time.Millisecond)).Progress(100, 1000)
	afterFirst := notifies
	for i := range 100 {
		at(tr, t0.Add(20*time.Millisecond)).Progress(int64(100+i), 1000)
	}
	if notifies != afterFirst {
		t.Fatalf("expected progress notifications to be throttled, got %d extra", notifies-afterFirst)
	}
	if afterFirst < base {
		t.Fatal("unreachable")
	}

	// After the throttle window, progress notifies again.
	at(tr, t0.Add(10*time.Second)).Progress(500, 1000)
	if notifies == afterFirst {
		t.Fatal("expected a notification after the throttle window")
	}

	a := tr.Get()
	if a.Transfer == nil {
		t.Fatal("expected transfer progress")
	}
	if a.Transfer.Sent != 500 || a.Transfer.Total != 1000 {
		t.Fatalf("expected 500/1000, got %+v", a.Transfer)
	}
	if pct := a.Transfer.Percent(); pct != 50 {
		t.Fatalf("expected 50%%, got %f", pct)
	}
	if a.Transfer.Rate <= 0 {
		t.Fatalf("expected a positive rate, got %f", a.Transfer.Rate)
	}
	if eta, ok := a.Transfer.ETA(); !ok || eta <= 0 {
		t.Fatalf("expected a positive ETA, got %v ok=%v", eta, ok)
	}

	// Starting a new step clears the transfer.
	tr.StartStep(2, 2, "delete")
	if tr.Get().Transfer != nil {
		t.Fatal("expected StartStep to clear the transfer")
	}
}

func TestTransferMath(t *testing.T) {
	x := Transfer{Sent: 0, Total: 0}
	if x.Percent() != 0 {
		t.Error("zero total is 0%")
	}
	if _, ok := x.ETA(); ok {
		t.Error("no ETA without a rate")
	}
	done := Transfer{Sent: 10, Total: 10, Rate: 5}
	if _, ok := done.ETA(); ok {
		t.Error("no ETA when complete")
	}
	half := Transfer{Sent: 5, Total: 10, Rate: 5}
	if eta, ok := half.ETA(); !ok || eta != time.Second {
		t.Errorf("expected 1s ETA, got %v ok=%v", eta, ok)
	}
}
