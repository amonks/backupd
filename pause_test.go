package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/model"
)

// TestSyncDatasetPausedGeneratesPlanButSkipsExecution: a paused dataset is
// still refreshed and replanned — the dashboard keeps showing what would
// happen — but no ZFS mutations run and the dataset doesn't count as
// failed.
func TestSyncDatasetPausedGeneratesPlanButSkipsExecution(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("data/tank", snapA),
			row("data/tank", snapB),
			row("data/tank", snapC),
		}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
		}},
	}}

	conf := testConf()
	conf.Overrides["/foo"].Paused = true
	b := newTestBackupd(conf, local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB, snapC}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))

	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	for _, x := range []*fakeExecutor{local, remote} {
		if x.calledMatching("zfs destroy") || x.calledMatching("zfs send") {
			t.Errorf("expected no mutations on %s executor, got calls:\n%s", x.name, strings.Join(x.calls, "\n"))
		}
	}
	if remote.calledMatching("receive_resume_token") {
		t.Errorf("expected no resume handling while paused, got calls:\n%s", strings.Join(remote.calls, "\n"))
	}

	ds := b.state.Deref().GetDataset("/foo")
	if ds.Plan == nil || len(ds.Plan.Steps) == 0 {
		t.Fatal("expected a plan to be generated while paused")
	}
	for i, step := range ds.Plan.Steps {
		if step.Status != model.StepPending {
			t.Errorf("expected step %d to stay pending, got %v", i, step.Status)
		}
	}
}

// TestPauseTakesEffectAtStepBoundary: pausing mid-plan lets the in-flight
// step finish and stops before the next one.
func TestPauseTakesEffectAtStepBoundary(t *testing.T) {
	var b *Backupd
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("data/tank", snapA),
			row("data/tank", snapB),
			row("data/tank", snapC),
		}},
		{match: "zfs destroy", fn: func() {
			b.conf.Swap(func(c *config.Config) *config.Config {
				c2 := *c
				c2.Paused = true
				return &c2
			})
		}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
		}},
	}}

	b = newTestBackupd(testConf(), local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB, snapC}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))

	// The plan is [delete local B, transfer A→C]. The deletion's side
	// effect pauses globally, so the transfer must not start.
	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	if !local.calledMatching("zfs destroy") {
		t.Errorf("expected the first step to run, got calls:\n%s", strings.Join(local.calls, "\n"))
	}
	if local.calledMatching("zfs send") {
		t.Errorf("expected no transfer after pause, got calls:\n%s", strings.Join(local.calls, "\n"))
	}
}

func steadyStateExecutors() (*fakeExecutor, *fakeExecutor) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t filesystem", rows: []string{"data/tank/foo\t0\t0"}},
		{match: "-t snapshot", rows: []string{row("data/tank", snapA)}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t filesystem", rows: []string{"backup/tank/foo\t0\t0"}},
		{match: "-t snapshot", rows: []string{row("backup/tank", snapA)}},
	}}
	return local, remote
}

// TestGlobalPauseSuppressesSnitch: a paused system is not a backed-up
// system, so the dead-man's-switch ping is withheld while paused.
func TestGlobalPauseSuppressesSnitch(t *testing.T) {
	local, remote := steadyStateExecutors()
	conf := testConf()
	conf.SnitchID = "test-snitch"
	conf.Paused = true
	b := newTestBackupd(conf, local, remote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snitched bool
	b.snitch = func(string) error { snitched = true; return nil }
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		cancel()
		return nil, ctx.Err()
	}

	if err := b.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return on cancellation, got: %v", err)
	}
	if snitched {
		t.Error("expected snitch ping to be suppressed while globally paused")
	}
}

func TestSnitchPingsWhenNotPaused(t *testing.T) {
	local, remote := steadyStateExecutors()
	conf := testConf()
	conf.SnitchID = "test-snitch"
	b := newTestBackupd(conf, local, remote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var snitched bool
	b.snitch = func(string) error { snitched = true; return nil }
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		cancel()
		return nil, ctx.Err()
	}

	if err := b.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return on cancellation, got: %v", err)
	}
	if !snitched {
		t.Error("expected snitch ping after a successful unpaused cycle")
	}
}

// TestSyncNowGlobalWakesIdle: a global sync-now request interrupts the
// between-cycles wait immediately.
func TestSyncNowGlobalWakesIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, remote := steadyStateExecutors()
		b := newTestBackupd(testConf(), local, remote)

		if !b.TriggerSync(true, "") {
			t.Fatal("TriggerSync returned false")
		}
		start := time.Now()
		if err := b.idle(context.Background(), 10*time.Second, 0); err != nil {
			t.Fatalf("idle: %v", err)
		}
		// On the bubble's fake clock an immediate wake takes exactly zero time.
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("expected idle to wake immediately, took %s", elapsed)
		}
	})
}

// TestSyncNowDatasetSyncsDuringIdle: a per-dataset request syncs just that
// dataset and then keeps waiting out the interval.
func TestSyncNowDatasetSyncsDuringIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, remote := steadyStateExecutors()
		b := newTestBackupd(testConf(), local, remote)
		b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
		b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))

		if !b.TriggerSync(false, "/foo") {
			t.Fatal("TriggerSync returned false")
		}
		start := time.Now()
		if err := b.idle(context.Background(), 300*time.Millisecond, 0); err != nil {
			t.Fatalf("idle: %v", err)
		}
		// The dataset sync consumes no fake time, so idle must wait out
		// exactly the full interval.
		if elapsed := time.Since(start); elapsed != 300*time.Millisecond {
			t.Errorf("expected idle to wait out the interval after the dataset sync, returned after %s", elapsed)
		}
		if !local.calledMatching("-t snapshot") {
			t.Errorf("expected the dataset to be refreshed, got calls:\n%s", strings.Join(local.calls, "\n"))
		}
	})
}

const reloadTestConfig = `[local]
root = "data/tank"
[local.policy]
daily = 1

[remote]
root = "backup/tank"
[remote.policy]
daily = 1
`

func TestReloadConfigFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backupd.toml")
	if err := os.WriteFile(path, []byte(reloadTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	conf, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}

	local, remote := steadyStateExecutors()
	b := newTestBackupd(conf, local, remote)

	// A hand-edit is picked up.
	if err := os.WriteFile(path, []byte("paused = true\n"+reloadTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	b.reloadConfigFromDisk()
	if !b.conf.Deref().Paused {
		t.Error("expected reload to pick up paused=true")
	}

	// An invalid file keeps the current config.
	if err := os.WriteFile(path, []byte("paused = true\nnot valid toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	b.reloadConfigFromDisk()
	if !b.conf.Deref().Paused {
		t.Error("expected invalid reload to keep the previous config")
	}
}
