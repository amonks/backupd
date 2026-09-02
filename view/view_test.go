package view

import (
	"strings"
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

var (
	now  = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	boot = now.Add(-10 * time.Hour)
)

func snap(name string, at time.Time) *model.Snapshot {
	return &model.Snapshot{Dataset: "/foo", Name: name, CreatedAt: at.Unix()}
}

// testConf: daily retention on both sides, so both cadences are 24h
// and the staleness thresholds 48h.
func testConf() *config.Config {
	conf := &config.Config{}
	conf.Local.Policy = map[string]int{"daily": 1}
	conf.Remote.Policy = map[string]int{"daily": 1}
	return conf
}

// build assembles a model containing one dataset, /foo.
func build(local, remote []*model.Snapshot) *model.Model {
	state := model.New()
	state = model.AddLocalDataset("/foo", local, nil)(state)
	if remote != nil {
		state = model.AddRemoteDataset("/foo", remote, nil)(state)
	}
	return state
}

func testInput(state *model.Model, conf *config.Config) Input {
	return Input{
		State:    state,
		Conf:     conf,
		History:  history.New(),
		Activity: status.Activity{Phase: status.Idle},
		Now:      now,
		Boot:     boot,
	}
}

func getDS(t *testing.T, sys System, name model.DatasetName) Dataset {
	t.Helper()
	for _, ds := range sys.Datasets {
		if ds.Name == name {
			return ds
		}
	}
	t.Fatalf("dataset %s not in system view", name)
	return Dataset{}
}

func hasIssue(sys System, kind IssueKind) *Issue {
	for i := range sys.Issues {
		if sys.Issues[i].Kind == kind {
			return &sys.Issues[i]
		}
	}
	return nil
}

// TestAges: the assurance facts are derived from the inventory and the
// clock — not from run history — so they are correct even immediately
// after a restart.
func TestAges(t *testing.T) {
	a := snap("daily-a", now.Add(-26*time.Hour))
	b := snap("daily-b", now.Add(-time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{a, b}, []*model.Snapshot{a}), testConf()))
	ds := getDS(t, sys, "/foo")

	if !ds.HasLocal || !ds.HasRemote {
		t.Fatalf("expected snapshots on both sides, got %+v", ds)
	}
	if ds.Snapshotted != time.Hour {
		t.Errorf("expected snapshotted 1h ago, got %s", ds.Snapshotted)
	}
	if ds.BackedUp != 26*time.Hour {
		t.Errorf("expected backed up 26h ago, got %s", ds.BackedUp)
	}
	if ds.Depth != 26*time.Hour {
		t.Errorf("expected depth 26h, got %s", ds.Depth)
	}
	if ds.Lag != 25*time.Hour {
		t.Errorf("expected lag 25h, got %s", ds.Lag)
	}
	if ds.Cadence != 24*time.Hour {
		t.Errorf("expected 24h cadence, got %s", ds.Cadence)
	}
	if ds.BackupCadence != 24*time.Hour {
		t.Errorf("expected 24h backup cadence, got %s", ds.BackupCadence)
	}
	// 26h < 48h: within the 2× grace window, so healthy.
	if ds.Health != HealthOK || ds.BackupStale || ds.SnapshotsStale {
		t.Errorf("expected ok, got %s (%s) stale=%v/%v", ds.Health, ds.Reason, ds.SnapshotsStale, ds.BackupStale)
	}
}

func TestStaleBackup(t *testing.T) {
	a := snap("daily-a", now.Add(-49*time.Hour))
	b := snap("daily-b", now.Add(-time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{a, b}, []*model.Snapshot{a}), testConf()))
	ds := getDS(t, sys, "/foo")

	if !ds.BackupStale || ds.SnapshotsStale {
		t.Fatalf("expected only the backup to be stale, got %+v", ds)
	}
	if ds.Health != HealthAtRisk {
		t.Errorf("expected at risk, got %s", ds.Health)
	}
	if !strings.Contains(ds.Reason, "backup") {
		t.Errorf("expected reason to mention the backup, got %q", ds.Reason)
	}
	issue := hasIssue(sys, IssueStaleBackup)
	if issue == nil {
		t.Fatal("expected a stale-backup issue")
	}
	if issue.Severity != Critical {
		t.Errorf("expected critical, got %s", issue.Severity)
	}
	if issue.Dataset == nil || *issue.Dataset != "/foo" {
		t.Errorf("expected issue to name /foo, got %+v", issue.Dataset)
	}
	if sys.Verdict != SystemFailing {
		t.Errorf("expected system failing, got %s", sys.Verdict)
	}
}

func TestStaleSnapshots(t *testing.T) {
	a := snap("daily-a", now.Add(-50*time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{a}, []*model.Snapshot{a}), testConf()))
	ds := getDS(t, sys, "/foo")

	if !ds.SnapshotsStale {
		t.Fatalf("expected stale snapshots, got %+v", ds)
	}
	if ds.Health != HealthAtRisk {
		t.Errorf("expected at risk, got %s", ds.Health)
	}
	if hasIssue(sys, IssueStaleSnapshots) == nil {
		t.Fatal("expected a stale-snapshots issue")
	}
	if issue := hasIssue(sys, IssueStaleSnapshots); issue.Severity != Warning {
		t.Errorf("expected warning severity, got %s", issue.Severity)
	}
}

// TestNoCadenceNoStaleness: without a policy there is no expectation
// to measure against, so nothing is ever stale.
func TestNoCadenceNoStaleness(t *testing.T) {
	conf := &config.Config{}
	a := snap("daily-a", now.Add(-1000*time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{a}, []*model.Snapshot{a}), conf))
	ds := getDS(t, sys, "/foo")
	if ds.Cadence != 0 || ds.BackupCadence != 0 || ds.SnapshotsStale || ds.BackupStale {
		t.Fatalf("expected no staleness without a cadence, got %+v", ds)
	}
	if ds.Health != HealthOK {
		t.Errorf("expected ok, got %s (%s)", ds.Health, ds.Reason)
	}
}

// TestBackupCadenceFromRemotePolicy: the remote's freshness expectation
// comes from the *remote* policy — if local snapshots hourly but the
// remote only retains dailies, a day-old remote snapshot is exactly
// what the configuration asks for, not staleness.
func TestBackupCadenceFromRemotePolicy(t *testing.T) {
	conf := &config.Config{}
	conf.Local.Policy = map[string]int{"hourly": 6, "daily": 7}
	conf.Remote.Policy = map[string]int{"daily": 14}
	fresh := snap("hourly-a", now.Add(-30*time.Minute))
	daily := snap("daily-a", now.Add(-30*time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{daily, fresh}, []*model.Snapshot{daily}), conf))
	ds := getDS(t, sys, "/foo")

	if ds.Cadence != time.Hour {
		t.Errorf("expected 1h snapshot cadence, got %s", ds.Cadence)
	}
	if ds.BackupCadence != 24*time.Hour {
		t.Errorf("expected 24h backup cadence, got %s", ds.BackupCadence)
	}
	// 30h > 2×1h but < 2×24h: fine by the remote's own standard.
	if ds.BackupStale {
		t.Error("expected the daily-retaining remote to not be stale at 30h")
	}
	if ds.Health != HealthOK {
		t.Errorf("expected ok, got %s (%s)", ds.Health, ds.Reason)
	}

	// Past 2× the remote cadence it is stale.
	old := snap("daily-old", now.Add(-49*time.Hour))
	sys = Compute(testInput(build([]*model.Snapshot{old, fresh}, []*model.Snapshot{old}), conf))
	ds = getDS(t, sys, "/foo")
	if !ds.BackupStale || ds.Health != HealthAtRisk {
		t.Errorf("expected stale backup at 49h, got %s (%s)", ds.Health, ds.Reason)
	}
}

func TestUnreplicated(t *testing.T) {
	a := snap("daily-a", now.Add(-time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{a}, nil), testConf()))
	ds := getDS(t, sys, "/foo")

	if !ds.Unreplicated || ds.HasRemote {
		t.Fatalf("expected unreplicated, got %+v", ds)
	}
	if ds.Health != HealthAtRisk || ds.Reason != "never replicated" {
		t.Errorf("expected at risk (never replicated), got %s (%s)", ds.Health, ds.Reason)
	}
	issue := hasIssue(sys, IssueUnreplicated)
	if issue == nil || issue.Severity != Critical {
		t.Fatalf("expected a critical unreplicated issue, got %+v", issue)
	}
	if sys.Verdict != SystemFailing {
		t.Errorf("expected system failing, got %s", sys.Verdict)
	}
}

func planned(steps ...*model.PlanStep) func(*model.Model) *model.Model {
	return func(state *model.Model) *model.Model {
		state = state.Clone()
		state.SetPlan("/foo", &model.Plan{Steps: steps})
		return state
	}
}

func step(op model.Operation, st model.StepStatus) *model.PlanStep {
	s := model.NewPlanStep(op)
	s.Status = st
	return s
}

// TestHealthPrecedence: failing > at risk > paused > behind > ok, with
// syncing as an orthogonal activity flag rather than a health state.
func TestHealthPrecedence(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-26*time.Hour))
	b := snap("daily-b", now.Add(-time.Hour))
	deletion := &model.SnapshotDeletion{Location: model.Local, Snapshot: a}
	transfer := &model.SnapshotRangeTransfer{Start: a, End: b}
	state := build([]*model.Snapshot{a, b}, []*model.Snapshot{a})
	state = planned(step(deletion, model.StepPending), step(transfer, model.StepPending))(state)
	in := testInput(state, conf)

	// Pending work: behind.
	ds := getDS(t, Compute(in), "/foo")
	if ds.Health != HealthBehind {
		t.Fatalf("expected behind, got %s (%s)", ds.Health, ds.Reason)
	}
	if ds.PendingDeletions != 1 || ds.PendingTransfers != 1 {
		t.Fatalf("expected 1 deletion + 1 transfer pending, got %+v", ds)
	}
	if ds.Reason != "1 deletion · 1 transfer queued" {
		t.Fatalf("unexpected reason %q", ds.Reason)
	}
	// Behind is routine, not an issue.
	if len(Compute(in).Issues) != 0 {
		t.Fatalf("expected no issues for behind, got %+v", Compute(in).Issues)
	}

	// A failure newer than the last success dominates.
	in.History.RecordDatasetFailure("/foo", now.Add(-time.Minute), "out of space")
	ds = getDS(t, Compute(in), "/foo")
	if ds.Health != HealthFailing || ds.Reason != "out of space" {
		t.Fatalf("expected failing(out of space), got %s (%s)", ds.Health, ds.Reason)
	}
	issue := hasIssue(Compute(in), IssueFailing)
	if issue == nil || issue.Severity != Critical || issue.Detail != "out of space" {
		t.Fatalf("expected a critical failing issue with the error, got %+v", issue)
	}
	if !issue.Since.Equal(now.Add(-time.Minute)) {
		t.Errorf("expected issue since the failure time, got %s", issue.Since)
	}

	// A newer success clears it.
	in.History.RecordDatasetSuccess("/foo", now)
	ds = getDS(t, Compute(in), "/foo")
	if ds.Health != HealthBehind {
		t.Fatalf("expected behind after newer success, got %s", ds.Health)
	}

	// Paused beats behind.
	conf.Paused = true
	ds = getDS(t, Compute(in), "/foo")
	if ds.Health != HealthPaused || ds.Reason != "paused globally" {
		t.Fatalf("expected paused globally, got %s (%s)", ds.Health, ds.Reason)
	}
	conf.Paused = false

	// Syncing is not a verdict: health stays, the activity flag flips.
	in.Activity = status.Activity{Phase: status.Syncing, Dataset: "/foo", Step: 2, Steps: 3}
	ds = getDS(t, Compute(in), "/foo")
	if ds.Health != HealthBehind {
		t.Fatalf("expected health to stay behind while syncing, got %s", ds.Health)
	}
	if !ds.Syncing || ds.Step != 2 || ds.Steps != 3 {
		t.Fatalf("expected syncing step 2 of 3, got %+v", ds)
	}
}

// TestPausedStaleness: pausing stops transfers, so backup staleness is
// the operator's choice and is not flagged — but pausing does not stop
// snapshot creation, so snapshot staleness still is.
func TestPausedStaleness(t *testing.T) {
	conf := testConf()
	conf.Paused = true
	old := snap("daily-a", now.Add(-100*time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{old}, []*model.Snapshot{old}), conf))
	ds := getDS(t, sys, "/foo")
	if ds.BackupStale {
		t.Error("expected pause to exempt backup staleness")
	}
	if !ds.SnapshotsStale {
		t.Error("expected snapshot staleness to survive pause")
	}
	if ds.Health != HealthAtRisk {
		t.Errorf("expected at risk to beat paused, got %s (%s)", ds.Health, ds.Reason)
	}
}

func TestFulfillment(t *testing.T) {
	conf := &config.Config{}
	conf.Local.Policy = map[string]int{"daily": 2, "weekly": 1}
	conf.Remote.Policy = map[string]int{"daily": 1}
	d1 := snap("daily-a", now.Add(-3*time.Hour))
	d2 := snap("daily-b", now.Add(-2*time.Hour))
	d3 := snap("daily-c", now.Add(-time.Hour))
	sys := Compute(testInput(build([]*model.Snapshot{d1, d2, d3}, []*model.Snapshot{d1}), conf))
	ds := getDS(t, sys, "/foo")

	if len(ds.Fulfillment) != 2 {
		t.Fatalf("expected 2 fulfillment rows, got %+v", ds.Fulfillment)
	}
	daily, weekly := ds.Fulfillment[0], ds.Fulfillment[1]
	if daily.Periodicity != "daily" || weekly.Periodicity != "weekly" {
		t.Fatalf("expected daily then weekly, got %+v", ds.Fulfillment)
	}
	if daily.LocalWant != 2 || daily.LocalHave != 3 || daily.RemoteWant != 1 || daily.RemoteHave != 1 {
		t.Errorf("daily: expected L 3/2, R 1/1, got %+v", daily)
	}
	if weekly.LocalWant != 1 || weekly.LocalHave != 0 || weekly.RemoteWant != 0 || weekly.RemoteHave != 0 {
		t.Errorf("weekly: expected L 0/1, R 0/0, got %+v", weekly)
	}
}

func TestSystemVerdictAndIssues(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-2*time.Hour))
	in := testInput(build([]*model.Snapshot{a}, []*model.Snapshot{a}), conf)

	// Healthy: no issues, ok, empty reason.
	sys := Compute(in)
	if sys.Verdict != SystemOK || len(sys.Issues) != 0 || sys.Reason != "" {
		t.Fatalf("expected a quiet ok system, got %s (%s) %+v", sys.Verdict, sys.Reason, sys.Issues)
	}

	// A warning-only issue: attention, not failing.
	conf.SnitchID = "abc"
	sys = Compute(in) // never pinged, booted 10h ago, 1h interval
	issue := hasIssue(sys, IssueSnitchOverdue)
	if issue == nil || issue.Severity != Warning {
		t.Fatalf("expected a snitch-overdue warning, got %+v", sys.Issues)
	}
	if sys.Verdict != SystemAttention {
		t.Errorf("expected attention, got %s", sys.Verdict)
	}
	if !sys.SnitchOverdue || !sys.SnitchConfigured {
		t.Errorf("expected snitch flags set, got %+v", sys)
	}

	// A recent ping clears it.
	in.History.RecordSnitch(now.Add(-30 * time.Minute))
	sys = Compute(in)
	if hasIssue(sys, IssueSnitchOverdue) != nil {
		t.Fatal("expected no snitch issue after a recent ping")
	}
	if sys.Verdict != SystemOK {
		t.Errorf("expected ok, got %s (%s)", sys.Verdict, sys.Reason)
	}

	// A critical issue: failing, reason names the top issue.
	in.History.RecordDatasetFailure("/foo", now.Add(-time.Minute), "boom")
	sys = Compute(in)
	if sys.Verdict != SystemFailing {
		t.Errorf("expected failing, got %s", sys.Verdict)
	}
	if !strings.Contains(sys.Reason, "/foo") {
		t.Errorf("expected reason to name the dataset, got %q", sys.Reason)
	}

	// Issues are ordered most severe first.
	if len(sys.Issues) < 1 || sys.Issues[0].Severity != Critical {
		t.Errorf("expected the critical issue first, got %+v", sys.Issues)
	}

	// Pause wins over everything and is an info issue.
	conf.Paused = true
	sys = Compute(in)
	if sys.Verdict != SystemPaused {
		t.Errorf("expected paused, got %s", sys.Verdict)
	}
	if hasIssue(sys, IssuePaused) == nil {
		t.Error("expected a paused issue")
	}
	conf.Paused = false

	// A paused subtree is an info issue naming the subtree.
	conf.Overrides = map[string]*config.Override{"/foo": {Paused: true}}
	sys = Compute(in)
	issue = hasIssue(sys, IssuePaused)
	if issue == nil || issue.Dataset == nil || *issue.Dataset != "/foo" {
		t.Fatalf("expected a paused issue for /foo, got %+v", issue)
	}
	if sys.Verdict == SystemPaused {
		t.Error("a subtree pause must not read as a global pause")
	}
}

func TestCycleIssues(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-2*time.Hour))
	in := testInput(build([]*model.Snapshot{a}, []*model.Snapshot{a}), conf)

	// Backing off after failed cycles.
	in.Activity = status.Activity{Phase: status.BackingOff, ConsecutiveFailures: 2}
	sys := Compute(in)
	issue := hasIssue(sys, IssueCycleFailing)
	if issue == nil || issue.Severity != Critical {
		t.Fatalf("expected a critical cycle issue, got %+v", sys.Issues)
	}
	if !strings.Contains(issue.Summary, "2 cycles") {
		t.Errorf("expected the consecutive count, got %q", issue.Summary)
	}
	if sys.Verdict != SystemFailing {
		t.Errorf("expected failing, got %s", sys.Verdict)
	}

	// A failed most-recent cycle (not currently backing off) still
	// surfaces, with its error.
	in.Activity = status.Activity{Phase: status.Idle}
	in.History.RecordCycle(history.Cycle{StoppedAt: now.Add(-time.Minute), OK: false, Error: "refresh exploded"})
	sys = Compute(in)
	issue = hasIssue(sys, IssueCycleFailing)
	if issue == nil || issue.Detail != "refresh exploded" {
		t.Fatalf("expected the cycle error, got %+v", issue)
	}

	// A successful cycle clears it.
	in.History.RecordCycle(history.Cycle{StoppedAt: now, OK: true})
	if hasIssue(Compute(in), IssueCycleFailing) != nil {
		t.Fatal("expected no cycle issue after a good cycle")
	}
}

func TestCycleProgress(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-2*time.Hour))
	in := testInput(build([]*model.Snapshot{a}, []*model.Snapshot{a}), conf)
	in.Activity = status.Activity{
		Phase:   status.Syncing,
		Dataset: "/c",
		Queue: []status.QueueEntry{
			{Dataset: "/a", State: status.QueueDone},
			{Dataset: "/b", State: status.QueueFailed},
			{Dataset: "/c", State: status.QueueActive},
			{Dataset: "/d", State: status.QueueWaiting},
			{Dataset: "/e", State: status.QueueSkipped},
		},
	}
	sys := Compute(in)
	c := sys.Cycle
	if c.Total != 5 || c.Done != 1 || c.Failed != 1 || c.Skipped != 1 {
		t.Fatalf("unexpected cycle progress %+v", c)
	}
	if !c.HasActive || c.Active != "/c" {
		t.Fatalf("expected /c active, got %+v", c)
	}
	if c.Position != 4 {
		t.Errorf("expected position 4 (3 finished + 1 active), got %d", c.Position)
	}
}

func TestTotals(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-2*time.Hour))
	state := model.New()
	state = model.AddLocalDataset("/foo", []*model.Snapshot{a}, &model.DatasetSize{Used: 100, LogicalReferenced: 50})(state)
	state = model.AddRemoteDataset("/foo", []*model.Snapshot{a}, &model.DatasetSize{Used: 70})(state)
	state = model.AddLocalDataset("/bar", []*model.Snapshot{a}, &model.DatasetSize{Used: 100})(state)
	state = model.AddRemoteDataset("/bar", []*model.Snapshot{a}, nil)(state)

	sys := Compute(testInput(state, conf))
	if sys.DatasetCount != 2 {
		t.Errorf("expected 2 datasets, got %d", sys.DatasetCount)
	}
	if sys.LocalUsed != 200 || sys.RemoteUsed != 70 {
		t.Errorf("expected local=200 remote=70, got %d/%d", sys.LocalUsed, sys.RemoteUsed)
	}
	ds := getDS(t, sys, "/foo")
	if ds.LocalUsed != 100 || ds.RemoteUsed != 70 || ds.LocalCount != 1 || ds.RemoteCount != 1 {
		t.Errorf("unexpected dataset cost facts %+v", ds)
	}
}

// TestTotalsNested: zfs's used already counts a dataset's descendants,
// so the system total sums what each dataset holds on its own — a
// tracked tree totals to its root, however deep it goes.
func TestTotalsNested(t *testing.T) {
	conf := testConf()
	a := snap("daily-a", now.Add(-2*time.Hour))
	state := model.New()
	// The tracked tree's root is the dataset named "", as it ships.
	state = model.AddLocalDataset("", []*model.Snapshot{a}, &model.DatasetSize{Used: 100, UsedByChildren: 90})(state)
	state = model.AddLocalDataset("/tm", []*model.Snapshot{a}, &model.DatasetSize{Used: 90, UsedByChildren: 60})(state)
	state = model.AddLocalDataset("/tm/brigid", []*model.Snapshot{a}, &model.DatasetSize{Used: 60})(state)
	state = model.AddRemoteDataset("", []*model.Snapshot{a}, &model.DatasetSize{Used: 50, UsedByChildren: 45})(state)
	state = model.AddRemoteDataset("/tm", []*model.Snapshot{a}, &model.DatasetSize{Used: 45, UsedByChildren: 30})(state)
	state = model.AddRemoteDataset("/tm/brigid", []*model.Snapshot{a}, &model.DatasetSize{Used: 30})(state)

	sys := Compute(testInput(state, conf))
	if sys.LocalUsed != 100 || sys.RemoteUsed != 50 {
		t.Errorf("expected local=100 remote=50, got %d/%d", sys.LocalUsed, sys.RemoteUsed)
	}
	ds := getDS(t, sys, "/tm")
	if ds.LocalUsed != 90 || ds.RemoteUsed != 45 {
		t.Errorf("zfs's inclusive used is kept: %+v", ds)
	}
	if ds.LocalOwn != 30 || ds.RemoteOwn != 15 || ds.LocalBeneath() != 60 || ds.RemoteBeneath() != 30 {
		t.Errorf("own and beneath split used: %+v", ds)
	}
	var local int64
	for _, ds := range sys.Datasets {
		local += ds.LocalOwn
	}
	if local != sys.LocalUsed {
		t.Errorf("the column of own figures sums to the header: %d vs %d", local, sys.LocalUsed)
	}
}
