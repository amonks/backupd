package daemon

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/env"
	"monks.co/backupd/history"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

// fakeExecutor implements env.Executor. Each command is matched against the
// handlers in order by substring; the first match wins.
type fakeExecutor struct {
	name     string
	calls    []string
	handlers []fakeHandler
}

type fakeHandler struct {
	match string
	rows  []string
	err   error
	fn    func() // optional side effect, run on match
}

func (f *fakeExecutor) Exec(logger *logger.Logger, args ...string) ([]string, error) {
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	for _, h := range f.handlers {
		if strings.Contains(cmd, h.match) {
			if h.fn != nil {
				h.fn()
			}
			return h.rows, h.err
		}
	}
	return nil, fmt.Errorf("fake %s executor: no handler for %q", f.name, cmd)
}

func (f *fakeExecutor) Execf(logger *logger.Logger, s string, args ...any) ([]string, error) {
	return f.Exec(logger, strings.Fields(fmt.Sprintf(s, args...))...)
}

// Command records the invocation the way Exec does and returns a
// command that cannot run: these tests never execute a transfer pipe,
// and a fake that silently shelled out would be worse than one that
// fails loudly.
func (f *fakeExecutor) Command(args ...string) *exec.Cmd {
	f.calls = append(f.calls, strings.Join(args, " "))
	return exec.Command("false")
}

func (f *fakeExecutor) calledMatching(substr string) bool {
	for _, call := range f.calls {
		if strings.Contains(call, substr) {
			return true
		}
	}
	return false
}

func testConf() *config.Config {
	noBaseline := false
	conf := &config.Config{}
	conf.Local.Root = "data/tank"
	conf.Local.Policy = map[string]int{"daily": 1}
	conf.Remote.Root = "backup/tank"
	conf.Remote.Policy = map[string]int{"daily": 1}
	conf.Overrides = map[string]*config.Override{
		"/foo": {KeepBaseline: &noBaseline},
	}
	return conf
}

func newTestDaemon(conf *config.Config, local, remote *fakeExecutor) *Daemon {
	b := New(Options{
		Config: conf,
		Env: &env.Env{
			Local:  env.NewZFS(conf.Local.Root, local),
			Remote: env.NewZFS(conf.Remote.Root, remote),
		},
	})
	b.resume = func(context.Context, *logger.Logger, model.DatasetName, string) error {
		return fmt.Errorf("unexpected resume call")
	}
	b.snitch = func(context.Context, string) error { return nil }
	b.state.Reset(model.New())
	return b
}

func testSnap(name string, createdAt int64) *model.Snapshot {
	return &model.Snapshot{Dataset: "/foo", Name: name, CreatedAt: createdAt}
}

var (
	snapA = testSnap("daily-2026-07-01-01:00:00", 1000)
	snapB = testSnap("daily-2026-07-02-01:00:00", 2000)
	snapC = testSnap("daily-2026-07-03-01:00:00", 3000)
)

// row renders a snapshot as a `zfs list` output row for a fakeExecutor.
func row(prefix string, snap *model.Snapshot) string {
	return fmt.Sprintf("%s/foo@%s\t%d\t0", prefix, snap.Name, snap.CreatedAt)
}

// TestSyncRetriesWithBackoffWhenRemoteIsDown covers the connectivity crash
// loop: when the remote is unreachable (e.g. rsync.net refusing SSH), the
// sync loop must not return the error — under `daemon -r` supervision that
// meant an instant process restart and a new SSH attempt every ~5 seconds.
// Instead it retries in-process with exponential backoff, keeping the HTTP
// server and /snapshot endpoint alive, and exits only on cancellation.
func TestSyncRetriesWithBackoffWhenRemoteIsDown(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t filesystem", rows: []string{"data/tank/foo\t0\t0"}},
		{match: "-t snapshot", rows: []string{row("data/tank", snapA)}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "-t filesystem", err: fmt.Errorf("running 'ssh': exit status 255: kex_exchange_identification: read: Connection reset by peer")},
	}}

	b := newTestDaemon(testConf(), local, remote)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		waits = append(waits, d)
		if len(waits) >= 7 {
			cancel()
		}
		return nil, ctx.Err()
	}

	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return only on cancellation, got: %v", err)
	}

	want := []time.Duration{
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		30 * time.Minute,
		30 * time.Minute,
	}
	if len(waits) != len(want) {
		t.Fatalf("expected %d waits, got %d: %v", len(want), len(waits), waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d: expected %s, got %s", i, want[i], waits[i])
		}
	}
}

// TestSyncBackoffResetsAfterSuccessfulCycle: a successful cycle waits the
// normal hourly interval and resets the backoff, so the next failure starts
// again from the minimum delay.
func TestSyncBackoffResetsAfterSuccessfulCycle(t *testing.T) {
	failing := []fakeHandler{
		{match: "-t filesystem", err: fmt.Errorf("running 'ssh': exit status 255: kex_exchange_identification: read: Connection reset by peer")},
	}
	working := []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t filesystem", rows: []string{"backup/tank/foo\t0\t0"}},
		{match: "-t snapshot", rows: []string{row("backup/tank", snapA)}},
	}

	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t filesystem", rows: []string{"data/tank/foo\t0\t0"}},
		{match: "-t snapshot", rows: []string{row("data/tank", snapA)}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: failing}

	b := newTestDaemon(testConf(), local, remote)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		waits = append(waits, d)
		switch len(waits) {
		case 1:
			remote.handlers = working
		case 2:
			remote.handlers = failing
		case 3:
			cancel()
		}
		return nil, ctx.Err()
	}

	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return only on cancellation, got: %v", err)
	}

	if len(waits) != 3 {
		t.Fatalf("expected 3 waits, got %d: %v", len(waits), waits)
	}
	if waits[0] != time.Minute {
		t.Errorf("wait 0 (failure): expected 1m backoff, got %s", waits[0])
	}
	if waits[1] <= 30*time.Minute {
		t.Errorf("wait 1 (success): expected the hourly interval, got %s", waits[1])
	}
	if waits[2] != time.Minute {
		t.Errorf("wait 2 (failure after success): expected backoff reset to 1m, got %s", waits[2])
	}
}

// TestSyncDatasetPlansFromRefreshedInventory covers the stale-plan bug:
// syncDataset refreshes the dataset from ZFS, but used to generate the plan
// from the inventory as of the start of the cycle. If snapshots changed in
// between (here: C was created and transferred), the stale plan contains
// operations that fail validation against the refreshed state ("too late to
// transfer") and the dataset is skipped for a full cycle.
func TestSyncDatasetPlansFromRefreshedInventory(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("data/tank", snapA),
			row("data/tank", snapB),
			row("data/tank", snapC),
		}},
		{match: "zfs destroy", rows: nil},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
			row("backup/tank", snapC),
		}},
		{match: "zfs destroy", rows: nil},
	}}

	b := newTestDaemon(testConf(), local, remote)
	// Cycle-start state is stale: it predates snapshot C existing locally
	// and having been transferred to the remote.
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))

	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	wantLocal := "zfs destroy data/tank/foo@daily-2026-07-01-01:00:00%daily-2026-07-02-01:00:00"
	if !local.calledMatching(wantLocal) {
		t.Errorf("expected local range deletion %q, got calls:\n%s", wantLocal, strings.Join(local.calls, "\n"))
	}
	wantRemote := "zfs destroy backup/tank/foo@daily-2026-07-01-01:00:00"
	if !remote.calledMatching(wantRemote) {
		t.Errorf("expected remote deletion %q, got calls:\n%s", wantRemote, strings.Join(remote.calls, "\n"))
	}
}

// TestSyncDatasetResumeFailureStillDeletes covers the resume deadlock: a
// resume failure (e.g. the remote is out of space) must not block the plan's
// deletions, because those deletions may be exactly what frees the space the
// resume needs. Deletions run, transfers are skipped, and the resume error is
// still reported so the dataset is retried next cycle.
func TestSyncDatasetResumeFailureStillDeletes(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("data/tank", snapA),
			row("data/tank", snapB),
			row("data/tank", snapC),
		}},
		{match: "zfs destroy", rows: nil},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"sometoken"}},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
		}},
		{match: "zfs destroy", rows: nil},
	}}

	b := newTestDaemon(testConf(), local, remote)
	b.resume = func(context.Context, *logger.Logger, model.DatasetName, string) error {
		return fmt.Errorf("process error: 'to' command error: exit status 1 (cannot receive resume stream: out of space)")
	}
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB, snapC}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA}, nil))

	err := b.syncDataset(context.Background(), "/foo")
	if err == nil {
		t.Fatal("expected syncDataset to report the resume error, got nil")
	}
	if !strings.Contains(err.Error(), "out of space") {
		t.Errorf("expected resume error to be reported, got: %v", err)
	}

	// The plan is [delete local B, transfer A→C]: the deletion must have
	// run, and the transfer must not have been attempted.
	wantLocal := "zfs destroy data/tank/foo@daily-2026-07-02-01:00:00"
	if !local.calledMatching(wantLocal) {
		t.Errorf("expected local deletion %q, got calls:\n%s", wantLocal, strings.Join(local.calls, "\n"))
	}
	if local.calledMatching("zfs send") {
		t.Errorf("expected no transfer attempt, got calls:\n%s", strings.Join(local.calls, "\n"))
	}
}

// TestSyncDatasetUnresumableTransferIsAborted: when the interrupted send's
// snapshot no longer exists locally (deleted manually or by a policy change),
// the transfer can never resume. backupd must abort it on the remote and
// carry on with a fresh plan instead of failing every cycle.
func TestSyncDatasetUnresumableTransferIsAborted(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{
			row("data/tank", snapA),
			row("data/tank", snapB),
			row("data/tank", snapC),
		}},
		{match: "zfs destroy", rows: nil},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"sometoken"}},
		{match: "zfs receive -A", rows: nil},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
			row("backup/tank", snapC),
		}},
		{match: "zfs destroy", rows: nil},
	}}

	b := newTestDaemon(testConf(), local, remote)
	b.resume = func(context.Context, *logger.Logger, model.DatasetName, string) error {
		return fmt.Errorf("getting size of resume: running 'zfs': exit status 1: cannot resume send: 'data/tank/foo@daily-2026-07-02-01:00:00' used in the initial send no longer exists")
	}
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA, snapB, snapC}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA, snapC}, nil))

	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	if !remote.calledMatching("zfs receive -A backup/tank/foo") {
		t.Errorf("expected resumable transfer to be aborted, got calls:\n%s", strings.Join(remote.calls, "\n"))
	}
	wantLocal := "zfs destroy data/tank/foo@daily-2026-07-01-01:00:00%daily-2026-07-02-01:00:00"
	if !local.calledMatching(wantLocal) {
		t.Errorf("expected local range deletion %q, got calls:\n%s", wantLocal, strings.Join(local.calls, "\n"))
	}
}

// TestSyncDatasetDiscardsDivergentRemoteTip covers the /movies wedge: a
// snapshot backupd had already transferred was destroyed locally before
// the next cycle, leaving the remote's tip absent from local. Every
// incremental send is based on the remote's tip, so the dataset could
// never sync again — the planner emitted a transfer its own validation
// rejected, forever. The tip is now destroyed on the remote, and
// because that is the one thing backupd destroys that no retention
// policy asked it to, it is journaled by name.
func TestSyncDatasetDiscardsDivergentRemoteTip(t *testing.T) {
	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{row("data/tank", snapA)}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
			row("backup/tank", snapB), // transferred, then destroyed locally
		}},
		{match: "zfs destroy", rows: nil},
	}}

	b := newTestDaemon(testConf(), local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", []*model.Snapshot{snapA, snapB}, nil))

	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	want := "zfs destroy backup/tank/foo@" + snapB.Name
	if !remote.calledMatching(want) {
		t.Errorf("expected remote deletion %q, got calls:\n%s", want, strings.Join(remote.calls, "\n"))
	}
	if local.calledMatching("zfs destroy") {
		t.Errorf("expected no local deletion, got calls:\n%s", strings.Join(local.calls, "\n"))
	}

	var journaled []string
	for _, e := range b.history.Events() {
		if strings.Contains(e.Message, "discarded remote snapshot") {
			if e.Level != history.Warning {
				t.Errorf("expected the discard journaled at warning level, got %q", e.Level)
			}
			journaled = append(journaled, e.Message)
		}
	}
	if len(journaled) != 1 {
		t.Fatalf("expected exactly one discard journaled, got %d: %v", len(journaled), journaled)
	}
	if !strings.Contains(journaled[0], snapB.Name) {
		t.Errorf("expected the journal entry to name %s, got %q", snapB.Name, journaled[0])
	}
}

// TestSyncDatasetJournalsEveryDivergentSnapshotByName pins the promise
// the singleton case cannot: adjacent divergent snapshots are destroyed
// as one range operation, and the journal owes the operator every name
// in it — three of them here, so an entry reporting only the range's
// endpoints would elide the middle one. These are the only deletions no
// retention policy asked for.
func TestSyncDatasetJournalsEveryDivergentSnapshotByName(t *testing.T) {
	interior := testSnap("daily-2026-07-04-01:00:00", 4000)
	last := testSnap("daily-2026-07-05-01:00:00", 5000)
	orphans := []*model.Snapshot{snapB, interior, last}

	local := &fakeExecutor{name: "local", handlers: []fakeHandler{
		{match: "-t snapshot", rows: []string{row("data/tank", snapA)}},
	}}
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "receive_resume_token", rows: []string{"-"}},
		{match: "-t snapshot", rows: []string{
			row("backup/tank", snapA),
			row("backup/tank", snapB),
			row("backup/tank", interior),
			row("backup/tank", last),
		}},
		{match: "zfs destroy", rows: nil},
	}}

	b := newTestDaemon(testConf(), local, remote)
	b.state.Swap(model.AddLocalDataset("/foo", []*model.Snapshot{snapA}, nil))
	b.state.Swap(model.AddRemoteDataset("/foo", append([]*model.Snapshot{snapA}, orphans...), nil))

	if err := b.syncDataset(context.Background(), "/foo"); err != nil {
		t.Fatalf("syncDataset: %v", err)
	}

	want := fmt.Sprintf("zfs destroy backup/tank/foo@%s%%%s", snapB.Name, last.Name)
	if !remote.calledMatching(want) {
		t.Fatalf("expected remote range deletion %q, got calls:\n%s", want, strings.Join(remote.calls, "\n"))
	}

	var journaled []string
	for _, e := range b.history.Events() {
		if strings.Contains(e.Message, "discarded remote snapshot") {
			journaled = append(journaled, e.Message)
		}
	}
	if len(journaled) != 1 {
		t.Fatalf("expected exactly one discard journaled, got %d: %v", len(journaled), journaled)
	}
	if !strings.Contains(journaled[0], "discarded remote snapshots ") {
		t.Errorf("expected a plural entry, got %q", journaled[0])
	}
	for _, snap := range orphans {
		if !strings.Contains(journaled[0], snap.Name) {
			t.Errorf("expected the journal entry to name %s, got %q", snap.Name, journaled[0])
		}
	}
}
