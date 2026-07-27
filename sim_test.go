package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"monks.co/backupd/atom"
	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/sim"
	"monks.co/backupd/status"
	"monks.co/backupd/view"
)

// These tests run the complete daemon — sync loop, planner, resume
// handling — against the sim package's in-memory ZFS pair. They are
// the controlled-environment demonstration of the real system: the
// only substitutions are the environment itself, the snitch, and the
// between-cycle wait.

func newSimBackupd(conf *config.Config, s *sim.Sim) *Backupd {
	b := &Backupd{
		conf:       atom.New(conf),
		state:      atom.New[*model.Model](nil),
		globalLogs: logger.New("global"),
		env:        s,
		addr:       "127.0.0.1:0",
		version:    atom.New[int64](0),
		versionCh:  make(chan struct{}, 1),
		syncNow:    make(chan syncRequest, 16),
		history:    history.New(),
	}
	b.activity = status.New(b.notifyStateChange)
	s.OnProgress = b.activity.Progress
	b.resume = s.Resume
	b.snitch = func(string) error { return nil }
	b.wait = b.waitWake
	b.state.Reset(model.New())
	return b
}

// runCycles runs the sync loop for exactly n cycles by cancelling the
// context at the nth between-cycle wait.
func runCycles(t rapid.TB, b *Backupd, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		waits++
		if waits >= n {
			cancel()
		}
		return nil, ctx.Err()
	}
	if err := b.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to end by cancellation, got: %v", err)
	}
}

// simConf builds a config with the given global policies. With
// keepBaseline=false it installs a keep_baseline override for each
// named dataset (override keys match subtrees, so there is no single
// key meaning "everything").
func simConf(localPolicy, remotePolicy map[string]int, keepBaseline bool, datasets ...model.DatasetName) *config.Config {
	conf := &config.Config{}
	conf.Local.Root = "tank"
	conf.Local.Policy = localPolicy
	conf.Remote.Root = "backup"
	conf.Remote.Policy = remotePolicy
	if !keepBaseline {
		kb := false
		conf.Overrides = map[string]*config.Override{}
		for _, ds := range datasets {
			conf.Overrides[ds.Path()] = &config.Override{KeepBaseline: &kb}
		}
	}
	return conf
}

func simSnap(ds model.DatasetName, name string, at int64) *model.Snapshot {
	return &model.Snapshot{Dataset: ds, Name: name, CreatedAt: at, LogicalReferenced: 1 << 20}
}

// assertConverged verifies that a dataset's sim state is a fixed point:
// the planner, given the sim's ground truth, has nothing left to do.
func assertConverged(t rapid.TB, s *sim.Sim, conf *config.Config, name model.DatasetName) {
	t.Helper()
	inv := s.Inventory(name)
	lp, rp, kb := conf.PolicyFor(name.Path())
	target := model.CalculateTargetInventory(inv, lp, rp, kb)
	plan, err := model.CalculateTransitionPlan(inv, target)
	if err != nil {
		t.Fatalf("planning from sim state of %s: %v\n%s", name, err, s)
	}
	if len(plan.Steps) != 0 {
		var steps []string
		for _, step := range plan.Steps {
			steps = append(steps, step.String())
		}
		t.Fatalf("%s not converged; remaining plan:\n%s\nsim state:\n%s",
			name, strings.Join(steps, "\n"), s)
	}
}

func TestSimCycleConverges(t *testing.T) {
	s := sim.New()
	a, b_, c := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000), simSnap("/foo", "daily-t03", 3000)
	s.SeedLocal("/foo", a, b_, c)
	s.SeedRemote("/foo", a)

	conf := simConf(map[string]int{"daily": 2}, map[string]int{"daily": 2}, true)
	b := newSimBackupd(conf, s)
	runCycles(t, b, 2)

	assertConverged(t, s, conf, "/foo")
	inv := s.Inventory("/foo")
	if !inv.Remote.Has(c) {
		t.Errorf("expected newest snapshot on the remote:\n%s", s)
	}
	cycles := b.history.Cycles()
	if len(cycles) != 2 || !cycles[0].OK || !cycles[1].OK {
		t.Errorf("expected 2 OK cycles, got %+v", cycles)
	}
	if a := b.activity.Get(); a.Phase != status.Idle || a.ConsecutiveFailures != 0 {
		t.Errorf("expected Idle activity after a successful cycle, got %+v", a)
	}
	if ops := b.history.Ops(); len(ops) == 0 {
		t.Error("expected executed transfers in the op feed")
	}
	if _, ok := b.history.LastSuccess("/foo"); !ok {
		t.Error("expected a last-success record for /foo")
	}

	// A further cycle must be a no-op.
	s.ResetMutations()
	b2 := newSimBackupd(conf, s)
	runCycles(t, b2, 1)
	if muts := s.Mutations(); len(muts) != 0 {
		t.Errorf("expected no mutations in a converged cycle, got %v", muts)
	}
}

// TestSimInterruptedTransferResumes: a transfer cut off mid-stream
// leaves resume state on the remote; the next cycle resumes it to
// completion and converges.
func TestSimInterruptedTransferResumes(t *testing.T) {
	s := sim.New()
	a, b_ := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000)
	s.SeedLocal("/foo", a, b_)
	s.SeedRemote("/foo", a)
	s.InterruptNextTransfer(0.5)

	conf := simConf(map[string]int{"daily": 2}, map[string]int{"daily": 2}, true)
	b := newSimBackupd(conf, s)
	runCycles(t, b, 1)

	cycles := b.history.Cycles()
	if len(cycles) != 1 || cycles[0].OK {
		t.Fatalf("expected the interrupted cycle to fail, got %+v", cycles)
	}
	if inv := s.Inventory("/foo"); inv.Remote.Has(b_) {
		t.Fatalf("expected interrupted transfer to not complete:\n%s", s)
	}
	if a := b.activity.Get(); a.Phase != status.BackingOff || a.ConsecutiveFailures != 1 {
		t.Errorf("expected BackingOff activity after a failed cycle, got %+v", a)
	}
	if f, ok := b.history.LastFailure("/foo"); !ok || f.Error == "" {
		t.Errorf("expected a last-failure record for /foo, got %+v (ok=%v)", f, ok)
	}

	runCycles(t, b, 1)
	if inv := s.Inventory("/foo"); !inv.Remote.Has(b_) {
		t.Fatalf("expected resumed transfer to complete:\n%s", s)
	}
	assertConverged(t, s, conf, "/foo")
	found := false
	for _, m := range s.Mutations() {
		if strings.HasPrefix(m, "resume /foo@") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the transfer to be resumed, not restarted; mutations: %v", s.Mutations())
	}
}

// TestSimRemoteFullStillDeletes: when the remote is out of space, the
// plan's deletions still run — they may be what frees the space — while
// the transfer fails and the dataset is reported failed. Clearing the
// fault lets the next cycle converge.
func TestSimRemoteFullStillDeletes(t *testing.T) {
	s := sim.New()
	a, b_, c := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000), simSnap("/foo", "daily-t03", 3000)
	s.SeedLocal("/foo", a, b_, c)
	s.SeedRemote("/foo", a, b_)
	s.SetRemoteFull(true)

	// daily=1 with no baseline: remote should end up with just the
	// latest, so A must be deleted and C transferred.
	conf := simConf(map[string]int{"daily": 3}, map[string]int{"daily": 1}, false, "/foo")
	b := newSimBackupd(conf, s)
	runCycles(t, b, 1)

	cycles := b.history.Cycles()
	if len(cycles) != 1 || cycles[0].OK {
		t.Fatalf("expected cycle to fail while remote is full, got %+v", cycles)
	}
	inv := s.Inventory("/foo")
	if inv.Remote.Has(a) {
		t.Errorf("expected remote deletion to run despite the full remote:\n%s", s)
	}
	if inv.Remote.Has(c) {
		t.Errorf("expected transfer to fail while remote is full:\n%s", s)
	}

	s.SetRemoteFull(false)
	runCycles(t, b, 2)
	assertConverged(t, s, conf, "/foo")
}

// TestSimUnresumableTransferAborted: resume state whose source snapshot
// no longer exists locally can never complete; the daemon aborts it on
// the remote and proceeds with a fresh plan.
func TestSimUnresumableTransferAborted(t *testing.T) {
	s := sim.New()
	a, b_ := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000)
	gone := simSnap("/foo", "daily-gone", 1500)
	s.SeedLocal("/foo", a, b_)
	s.SeedRemote("/foo", a)
	s.SeedInterruptedTransfer("/foo", gone, 100)

	conf := simConf(map[string]int{"daily": 2}, map[string]int{"daily": 2}, true)
	b := newSimBackupd(conf, s)
	runCycles(t, b, 1)

	assertConverged(t, s, conf, "/foo")
	if inv := s.Inventory("/foo"); !inv.Remote.Has(b_) {
		t.Fatalf("expected fresh transfer after aborting unresumable state:\n%s", s)
	}
	aborted := false
	for _, m := range s.Mutations() {
		if m == "abort resumable /foo" {
			aborted = true
		}
	}
	if !aborted {
		t.Errorf("expected the unresumable transfer to be aborted; mutations: %v", s.Mutations())
	}
}

// TestSimPausedCycleExecutesNothing: while globally paused, the state
// is refreshed and plans are generated (the dashboard keeps showing
// what would happen) but nothing mutates and the snitch is withheld.
func TestSimPausedCycleExecutesNothing(t *testing.T) {
	s := sim.New()
	a, b_ := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000)
	s.SeedLocal("/foo", a, b_)
	s.SeedRemote("/foo", a)

	conf := simConf(map[string]int{"daily": 2}, map[string]int{"daily": 2}, true)
	conf.Paused = true
	conf.SnitchID = "test-snitch"
	b := newSimBackupd(conf, s)
	snitched := 0
	b.snitch = func(string) error { snitched++; return nil }
	runCycles(t, b, 1)

	if muts := s.Mutations(); len(muts) != 0 {
		t.Errorf("expected no mutations while paused, got %v", muts)
	}
	if snitched != 0 {
		t.Errorf("expected the snitch ping to be withheld while paused")
	}
	ds := b.state.Deref().GetDataset("/foo")
	if ds == nil || ds.Plan == nil || len(ds.Plan.Steps) == 0 {
		t.Errorf("expected a plan to be generated while paused")
	}
	cycles := b.history.Cycles()
	if len(cycles) != 1 || !cycles[0].Paused {
		t.Errorf("expected cycle to be recorded as paused, got %+v", cycles)
	}
}

// computeView derives the dashboard state from a live daemon exactly
// the way the HTTP layer does, with the clock pinned by the caller.
func computeView(b *Backupd, at time.Time) view.System {
	return view.Compute(view.Input{
		State:    b.state.Deref(),
		Conf:     b.conf.Deref(),
		History:  b.history,
		Activity: b.activity.Get(),
		Now:      at,
		Boot:     at.Add(-time.Minute),
	})
}

// TestSimDashboardInvariants drives the real daemon over the sim and
// asserts that what the dashboard would say tracks the truth: a
// converged fleet reads all-clear with the backed-up age equal to the
// newest remote snapshot's age; a remote outage reads as a failing
// system with a cycle issue; recovery clears it.
func TestSimDashboardInvariants(t *testing.T) {
	s := sim.New()
	now := time.Now()
	a := simSnap("/foo", "daily-a", now.Add(-3*time.Hour).Unix())
	b_ := simSnap("/foo", "daily-b", now.Add(-2*time.Hour).Unix())
	c := simSnap("/foo", "daily-c", now.Add(-time.Hour).Unix())
	s.SeedLocal("/foo", a, b_, c)
	s.SeedRemote("/foo", a)

	conf := simConf(map[string]int{"daily": 3}, map[string]int{"daily": 3}, true)
	b := newSimBackupd(conf, s)
	runCycles(t, b, 2)

	sys := computeView(b, now)
	if sys.Verdict != view.SystemOK || len(sys.Issues) != 0 {
		t.Fatalf("expected a quiet ok system after convergence, got %s %+v", sys.Verdict, sys.Issues)
	}
	ds, ok := sys.Get("/foo")
	if !ok || ds.Health != view.HealthOK {
		t.Fatalf("expected /foo ok, got %+v", ds)
	}
	// Ground truth: the dashboard's backed-up age is the sim's newest
	// remote snapshot's age, to the second.
	newestRemote := s.Inventory("/foo").Remote.Newest()
	if want := now.Sub(newestRemote.Time()); ds.BackedUp != want {
		t.Errorf("backed-up age %s != sim ground truth %s", ds.BackedUp, want)
	}
	// The cycle queue accounts for every dataset, all done.
	queue := b.activity.Get().Queue
	if len(queue) != 1 || queue[0].Dataset != "/foo" || queue[0].State != status.QueueDone {
		t.Errorf("expected a completed one-dataset queue, got %+v", queue)
	}

	// A remote outage: the next cycle fails, and the dashboard says so.
	s.SetRemoteDown(true)
	runCycles(t, b, 1)
	sys = computeView(b, now)
	if sys.Verdict != view.SystemFailing {
		t.Fatalf("expected failing during the outage, got %s (%s)", sys.Verdict, sys.Reason)
	}
	found := false
	for _, issue := range sys.Issues {
		if issue.Kind == view.IssueCycleFailing && issue.Severity == view.Critical {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a critical cycle-failing issue, got %+v", sys.Issues)
	}

	// Recovery: the next successful cycle clears the verdict.
	s.SetRemoteDown(false)
	runCycles(t, b, 1)
	sys = computeView(b, now)
	if sys.Verdict != view.SystemOK || len(sys.Issues) != 0 {
		t.Fatalf("expected recovery to clear the issues, got %s %+v", sys.Verdict, sys.Issues)
	}
}

// TestSimFailingDatasetSurfaces: a dataset whose receives fail
// persistently shows up as failing (with the error), raises exactly
// one failing issue, and is marked failed in the cycle queue — while a
// healthy sibling stays ok.
func TestSimFailingDatasetSurfaces(t *testing.T) {
	s := sim.New()
	now := time.Now()
	mk := func(ds model.DatasetName, name string, agoHours int) *model.Snapshot {
		return simSnap(ds, name, now.Add(-time.Duration(agoHours)*time.Hour).Unix())
	}
	s.SeedLocal("/ok", mk("/ok", "daily-a", 3), mk("/ok", "daily-b", 1))
	s.SeedRemote("/ok", mk("/ok", "daily-a", 3))
	s.SeedLocal("/bad", mk("/bad", "daily-a", 3), mk("/bad", "daily-b", 1))
	s.SeedRemote("/bad", mk("/bad", "daily-a", 3))
	s.SetDatasetError("/bad", "invalid backup stream (checksum mismatch)")

	conf := simConf(map[string]int{"daily": 2}, map[string]int{"daily": 2}, true)
	b := newSimBackupd(conf, s)
	runCycles(t, b, 1)

	sys := computeView(b, now)
	bad, _ := sys.Get("/bad")
	if bad.Health != view.HealthFailing || !strings.Contains(bad.Reason, "checksum mismatch") {
		t.Fatalf("expected /bad failing with the error, got %s (%s)", bad.Health, bad.Reason)
	}
	okDS, _ := sys.Get("/ok")
	if okDS.Health != view.HealthOK {
		t.Fatalf("expected /ok to stay ok, got %s (%s)", okDS.Health, okDS.Reason)
	}
	failing := 0
	for _, issue := range sys.Issues {
		if issue.Kind == view.IssueFailing {
			failing++
			if issue.Dataset == nil || *issue.Dataset != "/bad" {
				t.Errorf("failing issue names %v, want /bad", issue.Dataset)
			}
		}
	}
	if failing != 1 {
		t.Errorf("expected exactly one failing issue, got %d", failing)
	}
	states := map[model.DatasetName]status.QueueState{}
	for _, e := range b.activity.Get().Queue {
		states[e.Dataset] = e.State
	}
	if states["/bad"] != status.QueueFailed || states["/ok"] != status.QueueDone {
		t.Errorf("expected /bad failed and /ok done in the queue, got %+v", states)
	}

	// Clearing the fault heals the dataset on the next cycle.
	s.SetDatasetError("/bad", "")
	runCycles(t, b, 1)
	sys = computeView(b, now)
	bad, _ = sys.Get("/bad")
	if bad.Health != view.HealthOK {
		t.Fatalf("expected /bad to recover, got %s (%s)", bad.Health, bad.Reason)
	}
}

// TestRapidSimConvergence is the end-to-end property: for arbitrary
// pool states and policies, a few real sync cycles land every dataset
// on the planner's fixed point, after which a further cycle performs no
// mutations at all.
func TestRapidSimConvergence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := sim.New()
		datasets := []model.DatasetName{"/a", "/b"}
		n := rapid.IntRange(1, 2).Draw(t, "datasets")
		datasets = datasets[:n]

		for _, name := range datasets {
			label := strings.TrimPrefix(name.Path(), "/")
			count := rapid.IntRange(0, 12).Draw(t, label+"-n")
			var local, remote []*model.Snapshot
			var newestShared *model.Snapshot
			for i := range count {
				typ := rapid.SampledFrom([]string{"hourly", "daily", "weekly"}).Draw(t, fmt.Sprintf("%s-type-%d", label, i))
				hour := rapid.IntRange(0, 19).Draw(t, fmt.Sprintf("%s-hour-%d", label, i))
				snap := simSnap(name, fmt.Sprintf("%s-t%02d", typ, hour), int64(hour)*3600)
				onLocal := rapid.Bool().Draw(t, fmt.Sprintf("%s-local-%d", label, i))
				onRemote := rapid.Bool().Draw(t, fmt.Sprintf("%s-remote-%d", label, i))
				if onLocal {
					local = append(local, snap)
				}
				// The remote's contents arrived via transfers from
				// local, so remote-only snapshots newer than the sync
				// point don't occur; see genInventory in the model
				// package for the same constraint.
				if onRemote && onLocal {
					remote = append(remote, snap)
					if newestShared == nil || newestShared.Less(snap) {
						newestShared = snap
					}
				} else if onRemote {
					remote = append(remote, snap)
				}
			}
			// Enforce the constraints: some shared sync point when the
			// remote is non-empty, and no remote-only snapshot newer
			// than it.
			if newestShared == nil {
				remote = nil
			} else {
				var kept []*model.Snapshot
				for _, snap := range remote {
					isLocal := false
					for _, l := range local {
						if l.ID() == snap.ID() {
							isLocal = true
						}
					}
					if isLocal || snap.Less(newestShared) {
						kept = append(kept, snap)
					}
				}
				remote = kept
			}
			s.SeedLocal(name, local...)
			if len(remote) > 0 {
				s.SeedRemote(name, remote...)
			}
		}

		conf := simConf(
			genMainPolicy(t, "local"),
			genMainPolicy(t, "remote"),
			rapid.Bool().Draw(t, "keepBaseline"),
			datasets...,
		)
		b := newSimBackupd(conf, s)
		runCycles(t, b, 3)

		for _, name := range datasets {
			assertConverged(t, s, conf, name)
		}

		// The fixed point is quiescent: one more full cycle performs no
		// mutations.
		s.ResetMutations()
		runCycles(t, b, 1)
		if muts := s.Mutations(); len(muts) != 0 {
			t.Fatalf("expected a converged cycle to mutate nothing, got %v\n%s", muts, s)
		}

		// And the dashboard agrees: a converged fleet has nothing failing
		// and nothing behind (staleness may legitimately fire — the
		// generated snapshots have arbitrary ages).
		sys := computeView(b, time.Unix(20*3600, 0))
		for _, ds := range sys.Datasets {
			if ds.Health == view.HealthFailing || ds.Health == view.HealthBehind {
				t.Fatalf("converged dataset %s reads %s (%s)", ds.Name, ds.Health, ds.Reason)
			}
		}
	})
}

func genMainPolicy(t *rapid.T, label string) map[string]int {
	policy := map[string]int{}
	for _, typ := range []string{"hourly", "daily", "weekly"} {
		if rapid.Bool().Draw(t, label+"-has-"+typ) {
			policy[typ] = rapid.IntRange(0, 4).Draw(t, label+"-n-"+typ)
		}
	}
	return policy
}
