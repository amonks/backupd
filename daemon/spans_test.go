package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/env"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/sim"
)

// The spans are a contract with whatever reads them — the fleet's Logs
// board for backups among them — so these tests pin their names, their
// nesting, their attributes, and how each ends, over the sim.

// fakeTracer records every span started on it, and fails the test if
// any was not ended exactly once.
type fakeTracer struct {
	mu    sync.Mutex
	spans []*fakeSpan
}

type fakeSpan struct {
	tracer *fakeTracer
	name   string
	parent *fakeSpan
	attrs  map[string]any
	ends   int
	level  slog.Level
	err    error
}

type fakeSpanKey struct{}

func newFakeTracer(t *testing.T) *fakeTracer {
	f := &fakeTracer{}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.spans {
			if s.ends != 1 {
				t.Errorf("span %s (%v) ended %d times, want once", s.name, s.attrs, s.ends)
			}
		}
	})
	return f
}

func (f *fakeTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	parent, _ := ctx.Value(fakeSpanKey{}).(*fakeSpan)
	s := &fakeSpan{tracer: f, name: name, parent: parent, attrs: map[string]any{}}
	f.mu.Lock()
	f.spans = append(f.spans, s)
	f.mu.Unlock()
	return context.WithValue(ctx, fakeSpanKey{}, s), s
}

func (s *fakeSpan) Set(key string, value any) {
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	s.attrs[key] = value
}

func (s *fakeSpan) End(level slog.Level, err error) {
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	s.ends++
	s.level, s.err = level, err
}

// attr returns one of the span's attributes.
func (s *fakeSpan) attr(key string) (any, bool) {
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	v, ok := s.attrs[key]
	return v, ok
}

// all returns every span, in the order they started.
func (f *fakeTracer) all() []*fakeSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.spans)
}

// named returns the spans of one name, in the order they started.
func (f *fakeTracer) named(name string) []*fakeSpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeSpan
	for _, s := range f.spans {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// one returns the only span of a name, failing the test otherwise.
func (f *fakeTracer) one(t *testing.T, name string) *fakeSpan {
	t.Helper()
	spans := f.named(name)
	if len(spans) != 1 {
		t.Fatalf("%d %s spans, want 1", len(spans), name)
	}
	return spans[0]
}

// steps returns the step spans — transfers and deletions — of a
// dataset, in the order they started.
func (f *fakeTracer) steps(name string, dataset model.DatasetName) []*fakeSpan {
	var out []*fakeSpan
	for _, s := range f.named(name) {
		if v, _ := s.attr(DatasetKey); v == dataset.String() {
			out = append(out, s)
		}
	}
	return out
}

// tracedSimDaemon runs the daemon over e with a recording tracer.
func tracedSimDaemon(t *testing.T, conf *config.Config, e env.Interface) (*Daemon, *fakeTracer) {
	t.Helper()
	tracer := newFakeTracer(t)
	b := New(Options{Config: conf, Env: e, Tracer: tracer})
	b.snitch = func(context.Context, string) error { return nil }
	b.state.Reset(model.New())
	return b, tracer
}

// checkSpan checks a span's attributes, and that it ended well.
func checkSpan(t *testing.T, s *fakeSpan, parent *fakeSpan, want map[string]any) {
	t.Helper()
	if s.parent != parent {
		t.Errorf("%s: parent %v, want %v", s.name, s.parent, parent)
	}
	if s.err != nil {
		t.Errorf("%s: ended with %v, want success", s.name, s.err)
	}
	for key, w := range want {
		if got, ok := s.attr(key); !ok || got != w {
			t.Errorf("%s: %s = %v (present %v), want %v", s.name, key, got, ok, w)
		}
	}
}

// A healthy cycle is one span, with every step it executed beneath it:
// an initial transfer into a dataset the remote does not have, the
// incremental transfer of a dataset already replicated, and the
// deletions retention asks for, on each side, with their counts.
func TestCycleSpanAndItsSteps(t *testing.T) {
	s := sim.New()
	s.SeedLocal("/foo",
		simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000),
		simSnap("/foo", "daily-t03", 3000), simSnap("/foo", "daily-t04", 4000))
	s.SeedRemote("/foo",
		simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000),
		simSnap("/foo", "daily-t03", 3000))
	s.SeedLocal("/new", simSnap("/new", "daily-t01", 1000))
	one := map[string]int{"daily": 1}
	b, tracer := tracedSimDaemon(t, simConf(one, one, false, "/foo"), s)
	runCycles(t, b, 1)

	cycle := tracer.one(t, CycleSpan)
	checkSpan(t, cycle, nil, map[string]any{CycleDatasetsKey: 2, CyclePausedKey: false})
	if v, ok := cycle.attr(CycleFailuresKey); ok {
		t.Errorf("a healthy cycle names failures: %v", v)
	}

	foo := tracer.steps(TransferSpan, "/foo")
	if len(foo) != 1 {
		t.Fatalf("%d transfers of /foo, want 1", len(foo))
	}
	checkSpan(t, foo[0], cycle, map[string]any{
		TransferKindKey:      TransferIncremental,
		TransferSentBytesKey: int64(1 << 20),
	})
	fresh := tracer.steps(TransferSpan, "/new")
	if len(fresh) != 1 {
		t.Fatalf("%d transfers of /new, want 1", len(fresh))
	}
	checkSpan(t, fresh[0], cycle, map[string]any{
		TransferKindKey:      TransferInitial,
		TransferSentBytesKey: int64(1 << 20),
	})

	destroys := tracer.steps(DestroySpan, "/foo")
	if len(destroys) != 2 {
		t.Fatalf("%d deletions in /foo, want one a side", len(destroys))
	}
	for i, loc := range []string{"local", "remote"} {
		checkSpan(t, destroys[i], cycle, map[string]any{
			DestroyLocationKey:  loc,
			DestroySnapshotsKey: 2,
		})
	}
	if muts := s.Mutations(); !slices.Contains(muts, "destroy Local /foo@daily-t02") || !slices.Contains(muts, "destroy Remote /foo@daily-t02") {
		t.Errorf("the deletions the spans count are not the ones the sim saw: %v", muts)
	}
}

// A deletion zfs refuses destroyed nothing, so its span counts nothing:
// the count is what the step destroyed, recorded once it has.
func TestRefusedDeletionCountsNothing(t *testing.T) {
	s := sim.New()
	s.SeedLocal("/foo",
		simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000),
		simSnap("/foo", "daily-t03", 3000), simSnap("/foo", "daily-t04", 4000))
	s.SeedRemote("/foo",
		simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000),
		simSnap("/foo", "daily-t03", 3000))
	one := map[string]int{"daily": 1}
	b, tracer := tracedSimDaemon(t, simConf(one, one, false, "/foo"), refusingRemoteDestroyEnv{s})
	runCycles(t, b, 1)

	destroys := tracer.steps(DestroySpan, "/foo")
	if len(destroys) != 2 {
		t.Fatalf("%d deletions in /foo, want one a side", len(destroys))
	}
	checkSpan(t, destroys[0], tracer.one(t, CycleSpan), map[string]any{
		DestroyLocationKey:  "local",
		DestroySnapshotsKey: 2,
	})
	refused := destroys[1]
	if loc, _ := refused.attr(DestroyLocationKey); loc != "remote" || refused.err == nil || refused.level != slog.LevelError {
		t.Errorf("the remote deletion ended (%s, %v) at %v, want an ERROR failure", refused.level, refused.err, loc)
	}
	if n, ok := refused.attr(DestroySnapshotsKey); ok {
		t.Errorf("a refused deletion says it destroyed %v snapshots", n)
	}
	if inv := s.Inventory("/foo"); inv.Remote.Len() != 3 {
		t.Errorf("the remote holds %d snapshots, want all 3: the deletion was refused", inv.Remote.Len())
	}
}

// refusingRemoteDestroyEnv is the sim, with zfs refusing every range
// deletion on the remote.
type refusingRemoteDestroyEnv struct{ *sim.Sim }

func (e refusingRemoteDestroyEnv) Apply(ctx context.Context, l *logger.Logger, op model.Operation) error {
	if step, ok := op.(*model.PlanStep); ok {
		op = step.Operation
	}
	if del, ok := op.(*model.SnapshotRangeDeletion); ok && del.Location == model.Remote {
		return errors.New("running 'ssh': exit status 1: cannot destroy snapshot backup/foo@daily-t01: dataset is busy")
	}
	return e.Sim.Apply(ctx, l, op)
}

// The rate a transfer records is the link's: the bytes the pipe moved
// between its first report of progress and its last, over the time
// between them, measured on the bubble's clock against a sim paced at
// 1.25 MiB a second whose receive then takes a second to finish — a
// second the pipe moved nothing in.
func TestTransferRateIsTheLinksRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sim.New()
		s.Rate = 5 << 18
		seedBehind(s, "/foo")
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), finalizingEnv{s})
		runOneCycle(t, b)

		xfer := tracer.one(t, TransferSpan)
		// The sim reports every tenth of a second, a tenth of the rate
		// each time: an eighth of the mebibyte has moved by the first
		// report, at 0.1s, and all of it by the last, at 0.8s. The first
		// report's eighth moved before the clock started, so the rate
		// is the other seven eighths over the 0.7s between: the sim's
		// own. The step ends at 1.8s, which the rate does not see.
		checkSpan(t, xfer, tracer.one(t, CycleSpan), map[string]any{
			TransferSentBytesKey:      int64(1 << 20),
			TransferBytesPerSecondKey: int64(5 << 18),
		})
	})
}

// finalizingEnv is the sim, with every plan step taking a second after
// its pipe is done: a receive committing what it was sent.
type finalizingEnv struct{ *sim.Sim }

func (e finalizingEnv) Apply(ctx context.Context, l *logger.Logger, op model.Operation) error {
	err := e.Sim.Apply(ctx, l, op)
	time.Sleep(time.Second)
	return err
}

// A cycle whose only failure is ssh failing to reach the remote is an
// expected failure, and its span says so: WARN, with the transport's
// error.type. Nothing ran, so nothing nests beneath it.
func TestWeatherFailedCycleSpanIsAWarning(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	s.SetRemoteDown(true)
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runCycles(t, b, 1)

	cycle := tracer.one(t, CycleSpan)
	if cycle.err == nil || cycle.level != slog.LevelWarn || errors.Is(cycle.err, context.Canceled) {
		t.Errorf("remote-down cycle ended (%s, %v), want a WARN failure", cycle.level, cycle.err)
	}
	if !strings.Contains(cycle.err.Error(), "Connection refused") {
		t.Errorf("error = %q, want ssh's refusal", cycle.err)
	}
	if kind, _ := cycle.attr("error.type"); kind != "connection-refused" {
		t.Errorf("error.type = %v, want connection-refused", kind)
	}
	if spans := tracer.all(); len(spans) != 1 {
		t.Errorf("%d spans, want the cycle alone", len(spans))
	}
}

// Steps are classified as the cycle is: a transfer whose ssh connection
// timed out is WARN, typed; one the remote rejects is a fault, ERROR,
// and the cycle with both in it is a fault too, naming both datasets.
func TestStepFailuresAreClassified(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/a", "/bad", "/ok")
	s.SetDatasetError("/a", sshConnectTimeout)
	s.SetDatasetError("/bad", "invalid backup stream (checksum mismatch)")
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runCycles(t, b, 1)

	cycle := tracer.one(t, CycleSpan)
	if cycle.err == nil || cycle.level != slog.LevelError {
		t.Fatalf("cycle ended (%s, %v), want an ERROR failure", cycle.level, cycle.err)
	}
	if failures, _ := cycle.attr(CycleFailuresKey); !slices.Equal(failures.([]string), []string{"/a", "/bad"}) {
		t.Errorf("%s = %v, want [/a /bad]", CycleFailuresKey, failures)
	}
	// The cycle ends with the fault that decided its level, for a host
	// to classify where the daemon did not, and says all of its
	// failures in error.message: a host reading the joined text would
	// find /a's timeout first, and name a fault weather.
	if msg := cycle.err.Error(); !strings.Contains(msg, "'/bad'") || strings.Contains(msg, "timed out") {
		t.Errorf("the cycle ended with %q, want /bad's fault alone", msg)
	}
	if msg, _ := cycle.attr("error.message"); !strings.Contains(fmt.Sprint(msg), "timed out") || !strings.Contains(fmt.Sprint(msg), "checksum mismatch") {
		t.Errorf("error.message = %q, want both failures", msg)
	}
	if kind, typed := cycle.attr("error.type"); typed {
		t.Errorf("error.type = %v, want none: the fault deciding the level is unclassified", kind)
	}

	for _, tc := range []struct {
		dataset model.DatasetName
		level   slog.Level
		kind    string
	}{
		{"/a", slog.LevelWarn, "timeout"},
		{"/bad", slog.LevelError, ""},
	} {
		steps := tracer.steps(TransferSpan, tc.dataset)
		if len(steps) != 1 {
			t.Fatalf("%d transfers of %s, want 1", len(steps), tc.dataset)
		}
		xfer := steps[0]
		if xfer.parent != cycle || xfer.err == nil || xfer.level != tc.level {
			t.Errorf("%s: ended (%s, %v) under %v, want a %s failure under the cycle", tc.dataset, xfer.level, xfer.err, xfer.parent, tc.level)
		}
		kind, typed := xfer.attr("error.type")
		if tc.kind == "" && typed || tc.kind != "" && kind != tc.kind {
			t.Errorf("%s: error.type = %v, want %q", tc.dataset, kind, tc.kind)
		}
		if sent, _ := xfer.attr(TransferSentBytesKey); sent != int64(0) {
			t.Errorf("%s: %s = %v, want 0: nothing moved", tc.dataset, TransferSentBytesKey, sent)
		}
		if rate, ok := xfer.attr(TransferBytesPerSecondKey); ok {
			t.Errorf("%s: a rate for a transfer that moved nothing: %v", tc.dataset, rate)
		}
	}
	checkSpan(t, tracer.steps(TransferSpan, "/ok")[0], cycle, nil)
}

// A cycle the daemon's stop cuts short ends as that cancellation — the
// cycle and the step it cut — and nothing is left open.
func TestCancelledCycleSpanIsEnded(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), stoppingEnv{Sim: s, stop: cancel})
	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	cycle := tracer.one(t, CycleSpan)
	xfer := tracer.one(t, TransferSpan)
	for _, span := range []*fakeSpan{cycle, xfer} {
		if !errors.Is(span.err, context.Canceled) {
			t.Errorf("%s ended with %v, want the cancellation", span.name, span.err)
		}
		if _, typed := span.attr("error.type"); typed {
			t.Errorf("%s: a cancellation classified as a failure", span.name)
		}
	}
	if xfer.parent != cycle {
		t.Errorf("the transfer's parent is %v, want the cycle", xfer.parent)
	}
	if len(b.history.Cycles()) != 0 {
		t.Errorf("a cancelled cycle was recorded: %+v", b.history.Cycles())
	}
}

// Dry-run executes nothing, so it starts no step: not the plan's
// transfers or deletions, not the resume of an interrupted transfer.
func TestDryRunStartsNoSteps(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	interrupted := simSnap("/r", "daily-t02", 2000)
	s.SeedLocal("/r", simSnap("/r", "daily-t01", 1000), interrupted)
	s.SeedRemote("/r", simSnap("/r", "daily-t01", 1000))
	s.SeedInterruptedTransfer("/r", interrupted, 100)
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	b.dryrun = true
	runCycles(t, b, 1)

	checkSpan(t, tracer.one(t, CycleSpan), nil, map[string]any{CycleDatasetsKey: 2})
	if spans := tracer.all(); len(spans) != 1 {
		var names []string
		for _, s := range spans {
			names = append(names, s.name)
		}
		t.Errorf("dry-run started %v, want the cycle alone", names)
	}
	if muts := s.Mutations(); len(muts) != 0 {
		t.Errorf("dry-run changed the sim: %v", muts)
	}
}

// An interrupted transfer fails with the bytes it moved, and the next
// cycle's resume is a transfer of its own kind, moving the rest.
func TestResumeSpan(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	s.InterruptNextTransfer(0.5)
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runCycles(t, b, 2)

	cycles := tracer.named(CycleSpan)
	if len(cycles) != 2 {
		t.Fatalf("%d cycles, want 2", len(cycles))
	}
	xfers := tracer.named(TransferSpan)
	if len(xfers) != 2 {
		t.Fatalf("%d transfers, want the interrupted one and its resume", len(xfers))
	}
	cut := xfers[0]
	if cut.parent != cycles[0] || cut.err == nil || cut.level != slog.LevelError {
		t.Errorf("the interrupted transfer ended (%s, %v), want an ERROR failure in the first cycle", cut.level, cut.err)
	}
	if sent, _ := cut.attr(TransferSentBytesKey); sent != int64(1<<19) {
		t.Errorf("the interrupted transfer moved %v, want half of it", sent)
	}
	checkSpan(t, xfers[1], cycles[1], map[string]any{
		DatasetKey:           "/foo",
		TransferKindKey:      TransferResume,
		TransferSentBytesKey: int64(1<<20 - 1<<19),
	})
	if abandoned, ok := xfers[1].attr(TransferAbandonedKey); ok {
		t.Errorf("a resume that finished is marked abandoned: %v", abandoned)
	}
}

// A resume that meets partially-complete state aborts it and tries
// again: two pipes, one span, every byte counted, and the rate over
// what each pipe moved between its first report and its last — not
// the abort and reconnect between them.
func TestResumeRetryIsOneSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sim.New()
		interrupted := simSnap("/foo", "daily-t02", 2000)
		s.SeedLocal("/foo", simSnap("/foo", "daily-t01", 1000), interrupted)
		s.SeedRemote("/foo", simSnap("/foo", "daily-t01", 1000))
		s.SeedInterruptedTransfer("/foo", interrupted, 100)
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
		progress := s.OnProgress
		calls := 0
		b.resume = func(context.Context, *logger.Logger, model.DatasetName, string) error {
			calls++
			if calls == 1 {
				progress(100, 1000)
				time.Sleep(time.Second)
				progress(300, 1000)
				return errors.New("cannot receive resume stream: destination contains partially-complete state from \"zfs receive -s\"")
			}
			time.Sleep(10 * time.Second)
			progress(100, 1000)
			time.Sleep(time.Second)
			progress(500, 1000)
			return nil
		}
		if err := b.refreshAllDatasetsAndPlans(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := b.handleIncompleteTransfer(t.Context(), b.globalLogs, "/foo"); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatalf("resumed %d times, want a retry after the abort", calls)
		}
		resumes := tracer.named(TransferSpan)
		if len(resumes) != 1 {
			t.Fatalf("%d transfer spans, want 1", len(resumes))
		}
		// 800 bytes sent. Between their first reports and their last,
		// the pipes moved 200 bytes and 400, a second apiece: 600 bytes
		// over two seconds.
		checkSpan(t, resumes[0], nil, map[string]any{
			TransferKindKey:           TransferResume,
			TransferSentBytesKey:      int64(800),
			TransferBytesPerSecondKey: int64(300),
		})
	})
}

// A resume whose snapshot is gone can never finish: the daemon aborts it
// on the remote, which is the resume's success, marked abandoned.
func TestAbandonedResumeSpan(t *testing.T) {
	s := sim.New()
	a, b_ := simSnap("/foo", "daily-t01", 1000), simSnap("/foo", "daily-t02", 2000)
	s.SeedLocal("/foo", a, b_)
	s.SeedRemote("/foo", a)
	s.SeedInterruptedTransfer("/foo", simSnap("/foo", "daily-gone", 1500), 100)
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	runCycles(t, b, 1)

	xfers := tracer.named(TransferSpan)
	if len(xfers) != 2 {
		t.Fatalf("%d transfers, want the abandoned resume and the fresh transfer", len(xfers))
	}
	checkSpan(t, xfers[0], tracer.one(t, CycleSpan), map[string]any{
		TransferKindKey:      TransferResume,
		TransferAbandonedKey: true,
		TransferSentBytesKey: int64(0),
	})
	checkSpan(t, xfers[1], tracer.one(t, CycleSpan), map[string]any{TransferKindKey: TransferIncremental})
}

// A snapshot taken on request is a span; one refused for its
// periodicity is the caller's mistake, and is none.
func TestSnapshotSpan(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)

	snapshot := func(periodicity string) int {
		rec := httptest.NewRecorder()
		b.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snapshot?periodicity="+periodicity, nil))
		return rec.Code
	}
	if code := snapshot("hourly"); code != http.StatusBadRequest {
		t.Fatalf("an unknown periodicity answered %d, want 400", code)
	}
	if len(tracer.all()) != 0 {
		t.Fatalf("a refused snapshot started a span")
	}
	if code := snapshot("daily"); code != http.StatusOK {
		t.Fatalf("a snapshot answered %d", code)
	}
	checkSpan(t, tracer.one(t, SnapshotSpan), nil, map[string]any{SnapshotPeriodicityKey: "daily"})
}

// A snapshot that fails is a fault: local zfs, nothing in the way.
func TestFailedSnapshotSpanIsAFault(t *testing.T) {
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), failingSnapshotEnv{sim.New()})
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snapshot?periodicity=daily", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed snapshot answered %d, want 500", rec.Code)
	}
	span := tracer.one(t, SnapshotSpan)
	if span.err == nil || span.level != slog.LevelError {
		t.Errorf("failed snapshot ended (%s, %v), want an ERROR failure", span.level, span.err)
	}
}

type failingSnapshotEnv struct{ *sim.Sim }

func (failingSnapshotEnv) Snapshot(context.Context, *logger.Logger, string, string) error {
	return errors.New("creating snapshot: cannot create snapshot 'tank@daily-x': out of space")
}

// A dataset synced on request between cycles is a span of its own, its
// steps beneath it, and its failure classified as a step's is.
func TestSingleDatasetSyncSpan(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo", "/a")
	s.SetDatasetError("/a", sshConnectTimeout)
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
	if err := b.refreshAllDatasetsAndPlans(t.Context()); err != nil {
		t.Fatal(err)
	}
	requests := []model.DatasetName{"/foo", "/a"}
	b.wait = func(context.Context, time.Duration) (*syncRequest, error) {
		if len(requests) == 0 {
			return nil, nil
		}
		req := &syncRequest{dataset: requests[0]}
		requests = requests[1:]
		return req, nil
	}
	if _, err := b.idle(t.Context(), time.Hour, 0); err != nil {
		t.Fatal(err)
	}

	syncs := tracer.named(SyncSpan)
	if len(syncs) != 2 {
		t.Fatalf("%d sync spans, want 2", len(syncs))
	}
	checkSpan(t, syncs[0], nil, map[string]any{DatasetKey: "/foo"})
	checkSpan(t, tracer.steps(TransferSpan, "/foo")[0], syncs[0], map[string]any{TransferKindKey: TransferIncremental})

	failed := syncs[1]
	if failed.err == nil || failed.level != slog.LevelWarn {
		t.Errorf("/a's sync ended (%s, %v), want a WARN failure", failed.level, failed.err)
	}
	if kind, _ := failed.attr("error.type"); kind != "timeout" {
		t.Errorf("/a's sync: error.type = %v, want timeout", kind)
	}
	if len(tracer.named(CycleSpan)) != 0 {
		t.Errorf("a requested sync started a cycle")
	}
}

// A snapshot whose request ends partway is no outcome: it ends
// cancelled, as the daemon's other work does, not as a fault.
func TestSnapshotCutShortIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), cancellingSnapshotEnv{Sim: sim.New(), cancel: cancel})
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/snapshot?periodicity=daily", nil))

	span := tracer.one(t, SnapshotSpan)
	if !errors.Is(span.err, context.Canceled) {
		t.Errorf("a snapshot whose request ended ended (%s, %v), want the cancellation", span.level, span.err)
	}
}

// cancellingSnapshotEnv is the sim, with the request ending while zfs
// takes the snapshot, which fails in its own words.
type cancellingSnapshotEnv struct {
	*sim.Sim
	cancel func()
}

func (e cancellingSnapshotEnv) Snapshot(context.Context, *logger.Logger, string, string) error {
	e.cancel()
	return errors.New("creating snapshot: signal: killed")
}

// A sync an operator asks for is the request's work: its span — the
// dataset's, or the cycle the request wakes — is the request's child,
// though the request returned long before the work ran. A cycle the
// interval started was asked for by nobody, and has no parent.
func TestRequestedSyncSpansAreTheRequestsChildren(t *testing.T) {
	t.Run("a dataset", func(t *testing.T) {
		s := sim.New()
		seedBehind(s, "/foo")
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
		if err := b.refreshAllDatasetsAndPlans(t.Context()); err != nil {
			t.Fatal(err)
		}
		request := askToSync(t, b, tracer, false, "/foo")
		b.wait = drainRequests(b, func() {})
		if _, err := b.idle(t.Context(), time.Hour, 0); err != nil {
			t.Fatal(err)
		}

		sync := tracer.one(t, SyncSpan)
		checkSpan(t, sync, request, map[string]any{DatasetKey: "/foo"})
		checkSpan(t, tracer.one(t, TransferSpan), sync, nil)
	})

	t.Run("a cycle", func(t *testing.T) {
		s := sim.New()
		seedBehind(s, "/foo")
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
		request := askToSync(t, b, tracer, true, "")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		b.wait = drainRequests(b, cancel)
		if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run: %v", err)
		}

		cycles := tracer.named(CycleSpan)
		if len(cycles) != 2 {
			t.Fatalf("%d cycles, want the first and the one requested", len(cycles))
		}
		checkSpan(t, cycles[0], nil, nil)
		checkSpan(t, cycles[1], request, nil)
	})
}

// The work a request asked for is still the sync loop's: Run's context
// ending cuts it short, and its spans end cancelled.
func TestRequestedSyncEndsWithRun(t *testing.T) {
	t.Run("a dataset", func(t *testing.T) {
		s := sim.New()
		seedBehind(s, "/foo")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), stoppingEnv{Sim: s, stop: cancel})
		if err := b.refreshAllDatasetsAndPlans(t.Context()); err != nil {
			t.Fatal(err)
		}
		request := askToSync(t, b, tracer, false, "/foo")
		b.wait = drainRequests(b, func() {})
		if _, err := b.idle(ctx, time.Hour, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("idle: %v", err)
		}

		sync, xfer := tracer.one(t, SyncSpan), tracer.one(t, TransferSpan)
		if sync.parent != request || xfer.parent != sync {
			t.Errorf("the sync's parent is %v and the transfer's %v, want the request and the sync", sync.parent, xfer.parent)
		}
		for _, span := range []*fakeSpan{sync, xfer} {
			if !errors.Is(span.err, context.Canceled) {
				t.Errorf("%s ended with %v, want the cancellation", span.name, span.err)
			}
		}
	})

	t.Run("a cycle", func(t *testing.T) {
		s := sim.New()
		seedBehind(s, "/foo")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), stoppingEnv{Sim: s, stop: cancel})
		request := askToSync(t, b, tracer, true, "")
		requested, err := b.idle(ctx, time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.cycle(ctx, requested); !errors.Is(err, context.Canceled) {
			t.Fatalf("cycle: %v", err)
		}

		cycle, xfer := tracer.one(t, CycleSpan), tracer.one(t, TransferSpan)
		if cycle.parent != request || xfer.parent != cycle {
			t.Errorf("the cycle's parent is %v and the transfer's %v, want the request and the cycle", cycle.parent, xfer.parent)
		}
		for _, span := range []*fakeSpan{cycle, xfer} {
			if !errors.Is(span.err, context.Canceled) {
				t.Errorf("%s ended with %v, want the cancellation", span.name, span.err)
			}
		}
	})
}

// askToSync makes a sync request the way POST /api/sync does, under a
// request span, and returns that span. The request returns at once,
// its context cancelled and its span ended, as a served request's are.
func askToSync(t *testing.T, b *Daemon, tracer *fakeTracer, all bool, dataset model.DatasetName) *fakeSpan {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ctx, request := tracer.Start(ctx, "request")
	if !b.TriggerSync(ctx, all, dataset) {
		t.Fatal("the sync queue is full")
	}
	cancel()
	request.End(slog.LevelInfo, nil)
	return request.(*fakeSpan)
}

// drainRequests is a wait that hands the loop each queued sync request
// at once, and when none is left calls empty and returns as an interval
// running out does — or, once empty has cancelled ctx, as a cancelled
// wait does.
func drainRequests(b *Daemon, empty func()) func(context.Context, time.Duration) (*syncRequest, error) {
	return func(ctx context.Context, _ time.Duration) (*syncRequest, error) {
		select {
		case req := <-b.syncNow:
			return &req, nil
		default:
		}
		empty()
		return nil, ctx.Err()
	}
}

// A host may run the daemon under a deadline. The work the deadline
// cuts short failed no more than work the daemon's stop cuts short, so
// its spans end cancelled — not as a failure at INFO, which is what a
// DeadlineExceeded would read as.
func TestDeadlineEndsSpansCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sim.New()
		s.Rate = 1 << 20
		seedBehind(s, "/foo")
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		if err := b.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run: %v", err)
		}

		for _, span := range []*fakeSpan{tracer.one(t, CycleSpan), tracer.one(t, TransferSpan)} {
			if !errors.Is(span.err, context.Canceled) {
				t.Errorf("%s ended with %v, want a cancellation", span.name, span.err)
			}
			if _, typed := span.attr("error.type"); typed {
				t.Errorf("%s: a cancellation classified as a failure", span.name)
			}
		}
	})
}

// A requested cycle is the sync loop's work as much as one the interval
// started, so a deadline on Run cuts it short the same way: Run returns
// the deadline, and the cycle and its transfer end cancelled, still the
// request's.
func TestRequestedCycleEndsWithRunsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := sim.New()
		s.Rate = 1 << 20
		seedBehind(s, "/foo")
		// The first cycle, which nobody asked for, finds the remote down
		// and fails at once; the requested one finds it back, and the
		// deadline comes halfway through its transfer.
		s.SetRemoteDown(true)
		b, tracer := tracedSimDaemon(t, simConf(dailyTwo, dailyTwo, true), s)
		request := askToSync(t, b, tracer, true, "")
		b.wait = func(ctx context.Context, _ time.Duration) (*syncRequest, error) {
			select {
			case req := <-b.syncNow:
				s.SetRemoteDown(false)
				return &req, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		if err := b.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run: %v, want the deadline", err)
		}

		cycles := tracer.named(CycleSpan)
		if len(cycles) != 2 {
			t.Fatalf("%d cycles, want the failed one and the one requested", len(cycles))
		}
		cycle, xfer := cycles[1], tracer.one(t, TransferSpan)
		if cycle.parent != request || xfer.parent != cycle {
			t.Errorf("the cycle's parent is %v and the transfer's %v, want the request and the cycle", cycle.parent, xfer.parent)
		}
		for _, span := range []*fakeSpan{cycle, xfer} {
			if !errors.Is(span.err, context.Canceled) {
				t.Errorf("%s ended with %v, want a cancellation", span.name, span.err)
			}
			if _, typed := span.attr("error.type"); typed {
				t.Errorf("%s: a cancellation classified as a failure", span.name)
			}
		}
	})
}

// runValueKey is a value a host puts on Run's context.
type runValueKey struct{}

// A requested cycle runs under Run's context with the span's values
// ahead of Run's: its check-in sees both the cycle's span, which is the
// request's child, and what the host put on Run's context — the same as
// a cycle the interval started.
func TestRequestedCycleChecksInWithRunsValues(t *testing.T) {
	s := sim.New()
	seedBehind(s, "/foo")
	conf := simConf(dailyTwo, dailyTwo, true)
	conf.SnitchID = "abc123"
	b, tracer := tracedSimDaemon(t, conf, s)
	type checkIn struct {
		run  any
		span *fakeSpan
	}
	var checkIns []checkIn
	b.snitch = func(ctx context.Context, _ string) error {
		span, _ := ctx.Value(fakeSpanKey{}).(*fakeSpan)
		checkIns = append(checkIns, checkIn{run: ctx.Value(runValueKey{}), span: span})
		return nil
	}
	request := askToSync(t, b, tracer, true, "")
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), runValueKey{}, "run's"))
	defer cancel()
	b.wait = drainRequests(b, cancel)
	if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	cycles := tracer.named(CycleSpan)
	if len(cycles) != 2 {
		t.Fatalf("%d cycles, want the first and the one requested", len(cycles))
	}
	checkSpan(t, cycles[1], request, nil)
	if len(checkIns) != 2 {
		t.Fatalf("%d check-ins, want one a cycle", len(checkIns))
	}
	for i, c := range checkIns {
		if c.run != "run's" {
			t.Errorf("cycle %d checked in seeing %v of Run's value, want it", i, c.run)
		}
		if c.span != cycles[i] {
			t.Errorf("cycle %d checked in under %v, want its own span", i, c.span)
		}
	}
}
