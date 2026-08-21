package history

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"monks.co/backupd/model"
)

var t0 = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func eventMessages(h *History) []string {
	var out []string
	for _, e := range h.Events() {
		out = append(out, e.Message)
	}
	return out
}

func TestEventsNewestFirstAndCapped(t *testing.T) {
	h := New()
	for i := range keepEvents + 25 {
		h.RecordEvent(Event{
			At:      t0.Add(time.Duration(i) * time.Minute),
			Level:   Info,
			Message: fmt.Sprintf("event-%d", i),
		})
	}
	events := h.Events()
	if len(events) != keepEvents {
		t.Fatalf("expected %d events, got %d", keepEvents, len(events))
	}
	if events[0].Message != fmt.Sprintf("event-%d", keepEvents+24) {
		t.Errorf("expected newest event first, got %s", events[0].Message)
	}
}

// The journal records incidents, not states: a dataset that fails for
// a week produces one entry — opened on the transition into failing,
// updated in place with a count and the latest error while the
// failure persists — and one entry when it recovers. The error text
// is allowed to drift (snapshot names, attempt counters, byte counts
// vary cycle to cycle) without producing new entries.
func TestDatasetFailureIncident(t *testing.T) {
	h := New()
	ds := model.DatasetName("/foo")

	h.RecordDatasetFailure(ds, t0, "transfer @daily-01: connection refused")
	if n := len(h.Events()); n != 1 {
		t.Fatalf("first failure should journal one event, got %d", n)
	}
	if e := h.Events()[0]; e.Level != Error || e.Dataset == nil || *e.Dataset != ds ||
		e.Message != "sync failing: transfer @daily-01: connection refused" {
		t.Errorf("unexpected event: %+v", e)
	}

	// Same incident, drifting error text: the entry is updated in
	// place, not duplicated, and keeps the incident's start time.
	h.RecordDatasetFailure(ds, t0.Add(time.Hour), "transfer @daily-02: connection refused")
	events := h.Events()
	if len(events) != 1 {
		t.Fatalf("persisting failure should not add entries, got %d: %v", len(events), eventMessages(h))
	}
	e := events[0]
	if e.Message != "sync failing (×2): transfer @daily-02: connection refused" {
		t.Errorf("expected updated incident message, got %q", e.Message)
	}
	if !e.At.Equal(t0) {
		t.Errorf("incident entry should keep its start time, got %s", e.At)
	}

	h.RecordDatasetFailure(ds, t0.Add(2*time.Hour), "out of space")
	if got := h.Events()[0].Message; got != "sync failing (×3): out of space" {
		t.Errorf("expected latest error in incident message, got %q", got)
	}

	// Recovery: one info entry naming the start of the streak, not the
	// most recent failure.
	h.RecordDatasetSuccess(ds, t0.Add(3*time.Hour))
	events = h.Events()
	if len(events) != 2 {
		t.Fatalf("recovery should journal, got %d events: %v", len(events), eventMessages(h))
	}
	if e := events[0]; e.Level != Info || !strings.Contains(e.Message, "recovered") ||
		!strings.Contains(e.Message, t0.Format("2006-01-02 15:04")) {
		t.Errorf("recovery event = %+v", e)
	}

	// Healthy successes are routine, not news.
	h.RecordDatasetSuccess(ds, t0.Add(4*time.Hour))
	if n := len(h.Events()); n != 2 {
		t.Fatalf("routine success should not journal, got %d events: %v", n, eventMessages(h))
	}

	// Failing again after recovery is a fresh incident, even with an
	// error already seen before.
	h.RecordDatasetFailure(ds, t0.Add(5*time.Hour), "out of space")
	if n := len(h.Events()); n != 3 {
		t.Fatalf("failure after recovery should journal, got %d events: %v", n, eventMessages(h))
	}
	if got := h.Events()[0].Message; got != "sync failing: out of space" {
		t.Errorf("fresh incident should start a fresh count, got %q", got)
	}
}

// Two datasets failing concurrently keep separate incident entries.
func TestConcurrentDatasetIncidents(t *testing.T) {
	h := New()
	h.RecordDatasetFailure("/a", t0, "err-a")
	h.RecordDatasetFailure("/b", t0.Add(time.Minute), "err-b")
	h.RecordDatasetFailure("/a", t0.Add(2*time.Minute), "err-a2")
	events := h.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 incident entries, got %d: %v", len(events), eventMessages(h))
	}
	// /b's entry is newer in the ring; /a's was updated in place.
	if *events[0].Dataset != "/b" || events[0].Message != "sync failing: err-b" {
		t.Errorf("events[0] = %+v", events[0])
	}
	if *events[1].Dataset != "/a" || events[1].Message != "sync failing (×2): err-a2" {
		t.Errorf("events[1] = %+v", events[1])
	}
}

// If an incident's entry has been pushed off the ring by other
// events, the next failure starts a fresh entry that keeps the count.
func TestIncidentEntryEvictedFromRing(t *testing.T) {
	h := New()
	ds := model.DatasetName("/foo")
	h.RecordDatasetFailure(ds, t0, "boom")
	for i := range keepEvents {
		h.RecordEvent(Event{At: t0.Add(time.Duration(i) * time.Second), Level: Info, Message: fmt.Sprintf("filler-%d", i)})
	}
	h.RecordDatasetFailure(ds, t0.Add(time.Hour), "boom")
	if got := h.Events()[0].Message; got != "sync failing (×2): boom" {
		t.Errorf("expected re-journaled incident, got %q", got)
	}
}

func TestFirstSuccessDoesNotJournal(t *testing.T) {
	h := New()
	h.RecordDatasetSuccess("/foo", t0)
	if n := len(h.Events()); n != 0 {
		t.Fatalf("first success should not journal, got %d events: %v", n, eventMessages(h))
	}
}

// Cycle incidents cover only cycle-level errors (refresh failures —
// the remote unreachable, say). Cycles that fail because datasets
// failed are already journaled by the dataset incidents.
func TestCycleIncident(t *testing.T) {
	h := New()
	cycle := func(i int, err string, failures ...string) Cycle {
		return Cycle{
			StartedAt: t0.Add(time.Duration(i) * time.Hour),
			StoppedAt: t0.Add(time.Duration(i)*time.Hour + time.Minute),
			OK:        err == "" && len(failures) == 0,
			Error:     err,
			Failures:  failures,
		}
	}

	h.RecordCycle(cycle(0, ""))
	if n := len(h.Events()); n != 0 {
		t.Fatalf("ok cycle should not journal, got %d: %v", n, eventMessages(h))
	}

	h.RecordCycle(cycle(1, "", "/foo"))
	if n := len(h.Events()); n != 0 {
		t.Fatalf("dataset-failure cycle should not journal (dataset incidents cover it), got %d: %v", n, eventMessages(h))
	}

	h.RecordCycle(cycle(2, "remote unreachable"))
	if n := len(h.Events()); n != 1 {
		t.Fatalf("cycle error should journal, got %d: %v", n, eventMessages(h))
	}
	if e := h.Events()[0]; e.Level != Error || e.Dataset != nil {
		t.Errorf("cycle event = %+v", e)
	}

	h.RecordCycle(cycle(3, "remote unreachable"))
	h.RecordCycle(cycle(4, "auth failed"))
	events := h.Events()
	if len(events) != 1 {
		t.Fatalf("persisting cycle error should update in place, got %d: %v", len(events), eventMessages(h))
	}
	if got := events[0].Message; got != "cycle failed (×3): auth failed" {
		t.Errorf("expected updated cycle incident, got %q", got)
	}

	h.RecordCycle(cycle(5, ""))
	events = h.Events()
	if len(events) != 2 {
		t.Fatalf("cycle recovery should journal, got %d: %v", len(events), eventMessages(h))
	}
	if e := events[0]; e.Level != Info || !strings.Contains(e.Message, "recovered") {
		t.Errorf("recovery event = %+v", e)
	}
}

func TestSnitchIncident(t *testing.T) {
	h := New()

	// Routine ping: no journal.
	h.RecordSnitch(t0)
	if n := len(h.Events()); n != 0 {
		t.Fatalf("routine snitch ping should not journal, got %d: %v", n, eventMessages(h))
	}

	h.RecordSnitchError(t0.Add(time.Hour), "504 gateway timeout")
	if n := len(h.Events()); n != 1 {
		t.Fatalf("first snitch error should journal, got %d: %v", n, eventMessages(h))
	}
	if e := h.Events()[0]; e.Level != Warning {
		t.Errorf("snitch error event = %+v", e)
	}

	h.RecordSnitchError(t0.Add(2*time.Hour), "504 gateway timeout")
	h.RecordSnitchError(t0.Add(3*time.Hour), "connection refused")
	events := h.Events()
	if len(events) != 1 {
		t.Fatalf("persisting snitch error should update in place, got %d: %v", len(events), eventMessages(h))
	}
	if got := events[0].Message; got != "snitch ping failing (×3): connection refused" {
		t.Errorf("expected updated snitch incident, got %q", got)
	}

	h.RecordSnitch(t0.Add(4 * time.Hour))
	events = h.Events()
	if len(events) != 2 {
		t.Fatalf("snitch recovery should journal, got %d: %v", len(events), eventMessages(h))
	}
	if e := events[0]; e.Level != Info || !strings.Contains(e.Message, "recovered") {
		t.Errorf("snitch recovery event = %+v", e)
	}
}
