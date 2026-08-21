package view

import (
	"time"

	"monks.co/backupd/history"
)

// CycleOutcome is the one-word result of a cycle: paused wins over
// everything (the operator chose it), then ok, then failed.
type CycleOutcome int

const (
	CycleOK CycleOutcome = iota
	CycleFailed
	CyclePaused
)

func (o CycleOutcome) String() string {
	switch o {
	case CycleFailed:
		return "failed"
	case CyclePaused:
		return "paused"
	default:
		return "ok"
	}
}

// CycleRun is a run of consecutive cycles with the same outcome and
// detail, collapsed so months of hourly checks read as a handful of
// rows — "ok × 300 over 12d" — instead of one indistinguishable row
// per cycle. A change in the failure detail starts a new run, so
// distinct errors stay distinct.
type CycleRun struct {
	Outcome CycleOutcome
	Count   int
	// First is the oldest member cycle's start; Last is the newest
	// member's stop.
	First       time.Time
	Last        time.Time
	AvgDuration time.Duration
	// Detail is the shared failure detail (the refresh error or the
	// failed-dataset list), empty for ok runs.
	Detail string
}

// OutcomeOf folds a cycle's flags into its one-word outcome.
func OutcomeOf(c history.Cycle) CycleOutcome {
	switch {
	case c.Paused:
		return CyclePaused
	case c.OK:
		return CycleOK
	default:
		return CycleFailed
	}
}

// CycleRuns collapses a newest-first cycle list into newest-first runs.
func CycleRuns(cycles []history.Cycle) []CycleRun {
	var runs []CycleRun
	var total time.Duration
	for _, c := range cycles {
		outcome, detail := OutcomeOf(c), CycleDetail(c)
		if n := len(runs); n > 0 && runs[n-1].Outcome == outcome && runs[n-1].Detail == detail {
			run := &runs[n-1]
			run.Count++
			run.First = c.StartedAt
			total += c.Duration()
			run.AvgDuration = total / time.Duration(run.Count)
			continue
		}
		total = c.Duration()
		runs = append(runs, CycleRun{
			Outcome:     outcome,
			Count:       1,
			First:       c.StartedAt,
			Last:        c.StoppedAt,
			AvgDuration: total,
			Detail:      detail,
		})
	}
	return runs
}
