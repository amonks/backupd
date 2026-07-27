package status

import (
	"testing"
	"time"

	"monks.co/backupd/model"
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

// TestQueue: the tracker records each cycle's dataset queue so the UI
// can answer "where in the cycle are we" — which datasets are done,
// which is active, which remain.
func TestQueue(t *testing.T) {
	tr := New(nil)
	t0 := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	at(tr, t0).StartCycle()
	if q := tr.Get().Queue; q != nil {
		t.Fatalf("expected no queue right after StartCycle, got %+v", q)
	}

	tr.SetQueue([]model.DatasetName{"/a", "/b", "/c"})
	q := tr.Get().Queue
	if len(q) != 3 {
		t.Fatalf("expected 3 queue entries, got %+v", q)
	}
	for i, want := range []model.DatasetName{"/a", "/b", "/c"} {
		if q[i].Dataset != want || q[i].State != QueueWaiting {
			t.Fatalf("entry %d: expected %s waiting, got %+v", i, want, q[i])
		}
	}

	// StartDataset marks the matching entry active.
	tr.StartDataset("/a")
	q = tr.Get().Queue
	if q[0].State != QueueActive {
		t.Fatalf("expected /a active, got %+v", q)
	}

	// FinishDataset records the outcome.
	tr.FinishDataset("/a", QueueDone)
	tr.StartDataset("/b")
	tr.FinishDataset("/b", QueueFailed)
	tr.StartDataset("/c")
	tr.FinishDataset("/c", QueueSkipped)
	q = tr.Get().Queue
	if q[0].State != QueueDone || q[1].State != QueueFailed || q[2].State != QueueSkipped {
		t.Fatalf("expected done/failed/skipped, got %+v", q)
	}

	// The queue survives the between-cycle wait (the UI may show the
	// completed cycle) and is cleared by the next StartCycle.
	tr.Wait(t0.Add(time.Hour), 0)
	if q := tr.Get().Queue; len(q) != 3 {
		t.Fatalf("expected Wait to preserve the queue, got %+v", q)
	}
	tr.StartCycle()
	if q := tr.Get().Queue; q != nil {
		t.Fatalf("expected StartCycle to clear the queue, got %+v", q)
	}

	// A single-dataset sync outside a full cycle appends its own entry.
	tr.StartDataset("/solo")
	q = tr.Get().Queue
	if len(q) != 1 || q[0].Dataset != "/solo" || q[0].State != QueueActive {
		t.Fatalf("expected a self-appended active entry, got %+v", q)
	}
	tr.FinishDataset("/solo", QueueDone)
	if q := tr.Get().Queue; q[0].State != QueueDone {
		t.Fatalf("expected /solo done, got %+v", q)
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
