package main

import (
	"fmt"
	"strings"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

// This file derives what the dashboard *says* from what the system
// *knows*: pure functions from (model, config, history, activity) to
// per-dataset and system-wide verdicts. Keeping the derivation out of
// the templates makes the judgment calls — what counts as failing,
// what counts as behind — testable.

type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictOK
	VerdictSyncing
	VerdictPending
	VerdictPaused
	VerdictFailing
)

// String is the operator-facing label.
func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictSyncing:
		return "syncing"
	case VerdictPending:
		return "behind"
	case VerdictPaused:
		return "paused"
	case VerdictFailing:
		return "failing"
	default:
		return "unknown"
	}
}

// Class is the CSS class suffix for verdict chips and dots.
func (v Verdict) Class() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictSyncing:
		return "syncing"
	case VerdictPending:
		return "pending"
	case VerdictPaused:
		return "paused"
	case VerdictFailing:
		return "failing"
	default:
		return "unknown"
	}
}

// DatasetView is everything the dashboard shows about one dataset's
// health, derived once per render.
type DatasetView struct {
	Name    model.DatasetName
	Verdict Verdict
	// Reason is a one-line explanation of a non-ok verdict.
	Reason string
	// Lag is how far the remote trails the local side (newest local
	// snapshot time minus newest remote snapshot time).
	Lag         time.Duration
	LastSuccess time.Time // zero if never
	LastFailure *history.Failure
	// Pending work, from the current plan's unexecuted steps.
	PendingDeletions int
	PendingTransfers int
	StepsDone        int
	StepsTotal       int
}

// PendingSummary describes queued work, e.g. "2 deletions · 1 transfer".
func (dv DatasetView) PendingSummary() string {
	var parts []string
	if dv.PendingDeletions > 0 {
		parts = append(parts, plural(dv.PendingDeletions, "deletion"))
	}
	if dv.PendingTransfers > 0 {
		parts = append(parts, plural(dv.PendingTransfers, "transfer"))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// datasetView derives one dataset's health. Verdict precedence:
// syncing (it's happening now) > failing (needs attention) > paused
// (intentionally stopped) > behind (work queued) > ok.
func datasetView(name model.DatasetName, ds *model.Dataset, conf *config.Config, hist *history.History, activity *status.Tracker) DatasetView {
	dv := DatasetView{Name: name}
	if ds == nil {
		return dv
	}
	dv.Lag = ds.Staleness()

	if at, ok := hist.LastSuccess(name); ok {
		dv.LastSuccess = at
	}
	if f, ok := hist.LastFailure(name); ok {
		dv.LastFailure = &f
	}

	if ds.Plan != nil {
		for _, step := range ds.Plan.Steps {
			switch step.Status {
			case model.StepCompleted:
				dv.StepsDone++
			case model.StepPending, model.StepInProgress, model.StepFailed:
				if model.IsTransfer(step.Operation) {
					dv.PendingTransfers++
				} else {
					dv.PendingDeletions++
				}
			}
		}
		dv.StepsTotal = len(ds.Plan.Steps)
	}

	failing := dv.LastFailure != nil &&
		(dv.LastSuccess.IsZero() || dv.LastFailure.At.After(dv.LastSuccess))

	switch {
	case activity.IsSyncing(name):
		dv.Verdict = VerdictSyncing
		if a := activity.Get(); a.Steps > 0 {
			dv.Reason = fmt.Sprintf("step %d of %d", a.Step, a.Steps)
		}
	case failing:
		dv.Verdict = VerdictFailing
		dv.Reason = dv.LastFailure.Error
	case conf.PausedFor(name.Path()):
		dv.Verdict = VerdictPaused
		if conf.Paused {
			dv.Reason = "paused globally"
		} else {
			dv.Reason = "paused"
		}
	case dv.PendingDeletions+dv.PendingTransfers > 0:
		dv.Verdict = VerdictPending
		dv.Reason = dv.PendingSummary() + " queued"
	default:
		dv.Verdict = VerdictOK
	}
	return dv
}

// SystemView is the top-of-page summary: one verdict for the whole
// daemon plus the numbers the status strip shows.
type SystemView struct {
	Verdict  Verdict
	Reason   string
	Datasets int
	Failing  int
	// Totals across datasets with known metrics.
	LocalUsed  int64
	RemoteUsed int64
	LastCycle  *history.Cycle
	LastSnitch time.Time // zero if never
}

// systemView derives the system verdict. Paused wins (the operator
// chose it and should be reminded); otherwise any failing dataset or a
// failed last cycle means attention is needed; otherwise ok.
func systemView(state *model.Model, conf *config.Config, hist *history.History, activity *status.Tracker, views []DatasetView) SystemView {
	sv := SystemView{Datasets: len(views)}

	for _, dv := range views {
		if dv.Verdict == VerdictFailing {
			sv.Failing++
		}
	}
	if state != nil {
		for _, name := range state.ListDatasets() {
			ds := state.GetDataset(name)
			if ds == nil {
				continue
			}
			if ds.Metrics.HasLocal {
				sv.LocalUsed += ds.Metrics.LocalSize.Used
			}
			if ds.Metrics.HasRemote {
				sv.RemoteUsed += ds.Metrics.RemoteSize.Used
			}
		}
	}
	if cycles := hist.Cycles(); len(cycles) > 0 {
		sv.LastCycle = &cycles[0]
	}
	if at, ok := hist.LastSnitch(); ok {
		sv.LastSnitch = at
	}

	a := activity.Get()
	switch {
	case conf.Paused:
		sv.Verdict = VerdictPaused
		sv.Reason = "execution is paused; the snitch ping is withheld"
	case sv.Failing > 0:
		sv.Verdict = VerdictFailing
		sv.Reason = plural(sv.Failing, "dataset") + " failing"
	case a.Phase == status.BackingOff:
		sv.Verdict = VerdictFailing
		sv.Reason = fmt.Sprintf("%s failed; retrying", plural(a.ConsecutiveFailures, "cycle"))
	case sv.LastCycle != nil && !sv.LastCycle.OK:
		sv.Verdict = VerdictFailing
		if sv.LastCycle.Error != "" {
			sv.Reason = sv.LastCycle.Error
		} else {
			sv.Reason = "last cycle failed"
		}
	default:
		sv.Verdict = VerdictOK
	}
	return sv
}
