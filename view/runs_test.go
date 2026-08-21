package view

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"monks.co/backupd/history"
)

var runsBase = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// mkCycles builds a newest-first cycle list from an oldest-first spec,
// mirroring how History returns cycles.
func mkCycles(specs []history.Cycle) []history.Cycle {
	out := make([]history.Cycle, len(specs))
	for i, c := range specs {
		c.StartedAt = runsBase.Add(time.Duration(i) * time.Hour)
		c.StoppedAt = c.StartedAt.Add(time.Duration(i+1) * time.Minute)
		out[len(specs)-1-i] = c
	}
	return out
}

func TestCycleRunsEmpty(t *testing.T) {
	if runs := CycleRuns(nil); len(runs) != 0 {
		t.Fatalf("expected no runs, got %d", len(runs))
	}
}

func TestCycleRunsCollapsesConsecutiveOK(t *testing.T) {
	cycles := mkCycles([]history.Cycle{
		{OK: true}, {OK: true}, {OK: true},
	})
	runs := CycleRuns(cycles)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %+v", len(runs), runs)
	}
	r := runs[0]
	if r.Outcome != CycleOK || r.Count != 3 {
		t.Errorf("run = %+v", r)
	}
	// First is the oldest cycle's start; Last the newest cycle's stop.
	if !r.First.Equal(runsBase) {
		t.Errorf("First = %s, want %s", r.First, runsBase)
	}
	if !r.Last.Equal(cycles[0].StoppedAt) {
		t.Errorf("Last = %s, want %s", r.Last, cycles[0].StoppedAt)
	}
	// Cycles took 1m, 2m, 3m.
	if want := 2 * time.Minute; r.AvgDuration != want {
		t.Errorf("AvgDuration = %s, want %s", r.AvgDuration, want)
	}
}

func TestCycleRunsSplitsOnOutcomeAndDetail(t *testing.T) {
	cycles := mkCycles([]history.Cycle{
		{OK: true},
		{OK: false, Error: "remote unreachable"},
		{OK: false, Error: "remote unreachable"},
		{OK: false, Error: "auth failed"},
		{OK: false, Failures: []string{"/foo"}},
		{OK: true, Paused: true},
		{OK: true},
	})
	runs := CycleRuns(cycles)
	if len(runs) != 6 {
		t.Fatalf("expected 6 runs, got %d: %+v", len(runs), runs)
	}
	// Newest first.
	wantOutcomes := []CycleOutcome{CycleOK, CyclePaused, CycleFailed, CycleFailed, CycleFailed, CycleOK}
	wantCounts := []int{1, 1, 1, 1, 2, 1}
	for i, r := range runs {
		if r.Outcome != wantOutcomes[i] || r.Count != wantCounts[i] {
			t.Errorf("run[%d] = %+v, want outcome %s count %d", i, r, wantOutcomes[i], wantCounts[i])
		}
	}
	if runs[2].Detail != "failed datasets: /foo" {
		t.Errorf("run[2].Detail = %q", runs[2].Detail)
	}
	if runs[3].Detail != "auth failed" {
		t.Errorf("run[3].Detail = %q", runs[3].Detail)
	}
}

// Paused wins over failure in the outcome label, matching the cycle
// table's precedence.
func TestCycleRunsPausedPrecedence(t *testing.T) {
	cycles := mkCycles([]history.Cycle{
		{OK: false, Paused: true, Error: "boom"},
	})
	runs := CycleRuns(cycles)
	if len(runs) != 1 || runs[0].Outcome != CyclePaused {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestCycleRunsProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 60).Draw(t, "n")
		specs := make([]history.Cycle, n)
		for i := range specs {
			switch rapid.IntRange(0, 3).Draw(t, "kind") {
			case 0:
				specs[i] = history.Cycle{OK: true}
			case 1:
				specs[i] = history.Cycle{OK: true, Paused: true}
			case 2:
				specs[i] = history.Cycle{OK: false, Error: rapid.SampledFrom([]string{"a", "b"}).Draw(t, "err")}
			case 3:
				specs[i] = history.Cycle{OK: false, Failures: []string{rapid.SampledFrom([]string{"/x", "/y"}).Draw(t, "ds")}}
			}
		}
		cycles := mkCycles(specs)
		runs := CycleRuns(cycles)

		// Counts partition the cycles.
		total := 0
		for _, r := range runs {
			if r.Count <= 0 {
				t.Fatalf("non-positive count: %+v", r)
			}
			total += r.Count
		}
		if total != len(cycles) {
			t.Fatalf("counts sum to %d, want %d", total, len(cycles))
		}

		// Adjacent runs differ — otherwise they would have merged.
		for i := 1; i < len(runs); i++ {
			if runs[i].Outcome == runs[i-1].Outcome && runs[i].Detail == runs[i-1].Detail {
				t.Fatalf("adjacent runs identical: %+v / %+v", runs[i-1], runs[i])
			}
		}

		// Expanding the runs reproduces each cycle's outcome and detail
		// in order.
		i := 0
		for _, r := range runs {
			for range r.Count {
				c := cycles[i]
				if OutcomeOf(c) != r.Outcome || CycleDetail(c) != r.Detail {
					t.Fatalf("cycle %d (%+v) not described by run %+v", i, c, r)
				}
				i++
			}
		}

		// Runs are newest-first and span their members.
		for _, r := range runs {
			if r.Last.Before(r.First) {
				t.Fatalf("run ends before it starts: %+v", r)
			}
		}
	})
}
