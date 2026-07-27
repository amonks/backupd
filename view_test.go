package main

import (
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

func viewFixtures() (*config.Config, *history.History, *status.Tracker) {
	conf := &config.Config{}
	conf.Local.Policy = map[string]int{"daily": 2}
	conf.Remote.Policy = map[string]int{"daily": 2}
	return conf, history.New(), status.New(nil)
}

func planned(steps ...*model.PlanStep) *model.Dataset {
	local := model.NewSnapshots(snapA, snapB)
	remote := model.NewSnapshots(snapA)
	return &model.Dataset{
		Name:    "/foo",
		Current: &model.SnapshotInventory{Local: local, Remote: remote},
		Plan:    &model.Plan{Steps: steps},
		Logs:    logger.New("/foo"),
	}
}

func step(op model.Operation, st model.StepStatus) *model.PlanStep {
	s := model.NewPlanStep(op)
	s.Status = st
	return s
}

func TestDatasetViewVerdictPrecedence(t *testing.T) {
	conf, hist, activity := viewFixtures()
	transfer := &model.SnapshotRangeTransfer{Start: snapA, End: snapB}
	deletion := &model.SnapshotDeletion{Location: model.Local, Snapshot: snapA}

	// Plain pending work: behind.
	ds := planned(step(deletion, model.StepPending), step(transfer, model.StepPending))
	dv := datasetView("/foo", ds, conf, hist, activity)
	if dv.Verdict != VerdictPending {
		t.Fatalf("expected behind, got %s", dv.Verdict)
	}
	if dv.PendingDeletions != 1 || dv.PendingTransfers != 1 {
		t.Fatalf("expected 1 deletion + 1 transfer pending, got %+v", dv)
	}
	if dv.PendingSummary() != "1 deletion · 1 transfer" {
		t.Fatalf("unexpected pending summary %q", dv.PendingSummary())
	}

	// A recorded failure newer than the last success dominates.
	hist.RecordDatasetFailure("/foo", time.Now(), "out of space")
	dv = datasetView("/foo", ds, conf, hist, activity)
	if dv.Verdict != VerdictFailing || dv.Reason != "out of space" {
		t.Fatalf("expected failing(out of space), got %s(%s)", dv.Verdict, dv.Reason)
	}

	// A success newer than the failure clears it.
	hist.RecordDatasetSuccess("/foo", time.Now().Add(time.Minute))
	dv = datasetView("/foo", ds, conf, hist, activity)
	if dv.Verdict != VerdictPending {
		t.Fatalf("expected behind after newer success, got %s", dv.Verdict)
	}

	// Paused beats pending (but not failing).
	conf.Paused = true
	dv = datasetView("/foo", ds, conf, hist, activity)
	if dv.Verdict != VerdictPaused || dv.Reason != "paused globally" {
		t.Fatalf("expected paused globally, got %s(%s)", dv.Verdict, dv.Reason)
	}
	conf.Paused = false

	// Actively syncing beats everything.
	activity.StartDataset("/foo")
	activity.StartStep(2, 3, "transfer")
	dv = datasetView("/foo", ds, conf, hist, activity)
	if dv.Verdict != VerdictSyncing || dv.Reason != "step 2 of 3" {
		t.Fatalf("expected syncing(step 2 of 3), got %s(%s)", dv.Verdict, dv.Reason)
	}

	// Nothing pending, no failures, not paused: ok.
	activity.Wait(time.Now().Add(time.Hour), 0)
	done := planned(step(deletion, model.StepCompleted))
	dv = datasetView("/foo", done, conf, hist, activity)
	if dv.Verdict != VerdictOK {
		t.Fatalf("expected ok, got %s(%s)", dv.Verdict, dv.Reason)
	}
	if dv.StepsDone != 1 || dv.StepsTotal != 1 {
		t.Fatalf("expected 1/1 steps done, got %+v", dv)
	}
}

func TestDatasetViewLag(t *testing.T) {
	conf, hist, activity := viewFixtures()
	ds := planned()
	dv := datasetView("/foo", ds, conf, hist, activity)
	want := snapB.Time().Sub(snapA.Time())
	if dv.Lag != want {
		t.Fatalf("expected lag %s, got %s", want, dv.Lag)
	}
}

func TestSystemViewVerdicts(t *testing.T) {
	conf, hist, activity := viewFixtures()
	state := model.New()

	// No data at all: ok (nothing is failing).
	sv := systemView(state, conf, hist, activity, nil)
	if sv.Verdict != VerdictOK {
		t.Fatalf("expected ok, got %s", sv.Verdict)
	}

	// A failing dataset view flips the system to failing.
	views := []DatasetView{{Name: "/foo", Verdict: VerdictFailing}}
	sv = systemView(state, conf, hist, activity, views)
	if sv.Verdict != VerdictFailing || sv.Failing != 1 {
		t.Fatalf("expected failing with 1 dataset, got %s (%d)", sv.Verdict, sv.Failing)
	}
	if sv.Reason != "1 dataset failing" {
		t.Fatalf("unexpected reason %q", sv.Reason)
	}

	// Backing off after failed cycles is failing even with no
	// per-dataset failure (e.g. the refresh itself failed).
	activity.Wait(time.Now().Add(time.Minute), 2)
	sv = systemView(state, conf, hist, activity, nil)
	if sv.Verdict != VerdictFailing || sv.Reason != "2 cycles failed; retrying" {
		t.Fatalf("expected failing(2 cycles failed; retrying), got %s(%s)", sv.Verdict, sv.Reason)
	}

	// Pause wins over everything: the operator chose it and must be
	// reminded that the snitch will eventually fire.
	conf.Paused = true
	sv = systemView(state, conf, hist, activity, views)
	if sv.Verdict != VerdictPaused {
		t.Fatalf("expected paused, got %s", sv.Verdict)
	}
	conf.Paused = false

	// A failed most-recent cycle is failing; a successful one is ok.
	activity.Wait(time.Now().Add(time.Hour), 0)
	hist.RecordCycle(history.Cycle{OK: false, Error: "boom"})
	sv = systemView(state, conf, hist, activity, nil)
	if sv.Verdict != VerdictFailing || sv.Reason != "boom" {
		t.Fatalf("expected failing(boom), got %s(%s)", sv.Verdict, sv.Reason)
	}
	hist.RecordCycle(history.Cycle{OK: true})
	sv = systemView(state, conf, hist, activity, nil)
	if sv.Verdict != VerdictOK {
		t.Fatalf("expected ok after a good cycle, got %s(%s)", sv.Verdict, sv.Reason)
	}
}

func TestSystemViewTotals(t *testing.T) {
	conf, hist, activity := viewFixtures()
	state := model.New()
	size := &model.DatasetSize{Used: 100, LogicalReferenced: 50}
	state = model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, size)(state)
	state = model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, &model.DatasetSize{Used: 70})(state)
	state = model.AddLocalDataset("/bar", []*model.Snapshot{snapA}, size)(state)

	sv := systemView(state, conf, hist, activity, []DatasetView{{}, {}})
	if sv.Datasets != 2 {
		t.Errorf("expected 2 datasets, got %d", sv.Datasets)
	}
	if sv.LocalUsed != 200 || sv.RemoteUsed != 70 {
		t.Errorf("expected local=200 remote=70, got local=%d remote=%d", sv.LocalUsed, sv.RemoteUsed)
	}
}
