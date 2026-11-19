package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"monks.co/backupd/atom"
	"monks.co/backupd/config"
	"monks.co/backupd/env"
	"monks.co/backupd/history"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/snitch"
	"monks.co/backupd/status"
)

type Daemon struct {
	conf  *atom.Atom[*config.Config]
	state *atom.Atom[*model.Model]
	// localMu makes local reads take turns, each published before the
	// next begins, so the model's local side is always the read that
	// started last: a read that began before a snapshot can never
	// publish over one that began after it. The cycle's listing, a
	// snapshot request's refresh, and a dataset's pre-sync refresh all
	// hold it; remote reads do not need it, since nothing else writes
	// the remote side while the cycle lists it.
	localMu    sync.Mutex
	globalLogs *logger.Logger
	activity   *status.Tracker
	env        env.Interface
	dryrun     bool

	// log is the structured record of what the daemon does — journal
	// entries, snitch pings, and, with no tracer, a record per span —
	// for a host that collects them. See Options.Logger.
	log *slog.Logger

	// tracer records the daemon's units of work as spans; nil writes a
	// record per span to log instead. See Options.Tracer and startSpan.
	tracer Tracer

	// meter measures the transfer in flight for its span, from the
	// progress callback New installs. One serves every transfer because
	// transfers never overlap: they run only inside syncDataset (a plan's
	// transfer steps, and the resume ahead of them), and the sync loop is
	// syncDataset's only caller — a cycle's syncAll, and idle's
	// requested syncs between cycles — so one transfer ends before the
	// next begins.
	meter transferMeter

	// layout and omitNav are how a host wraps the dashboard in its own
	// chrome. See Options.
	layout  Layout
	omitNav bool

	version   *atom.Atom[int64]
	versionCh chan struct{}
	syncNow   chan syncRequest
	history   *history.History
	// boot anchors "since boot" judgments (e.g. how overdue the snitch
	// ping is before any ping has happened).
	boot time.Time

	// lastConfigReloadError edge-guards the reload-failure journal
	// entry; only the sync loop touches it.
	lastConfigReloadError string

	// resume is env.Resume, injectable for tests.
	resume func(context.Context, *logger.Logger, model.DatasetName, string) error

	// snitch pings Dead Man's Snitch. Options supplies the host's
	// implementation; the standalone default lives in package snitch.
	snitch func(context.Context, string) error

	// wait blocks for a duration, returning early with a sync-now
	// request if one arrives; injectable for tests.
	wait func(context.Context, time.Duration) (*syncRequest, error)
}

// syncRequest asks the sync loop for an immediate sync: the whole cycle
// (all) or a single dataset.
type syncRequest struct {
	all     bool
	dataset model.DatasetName
	// ctx is the context the sync was asked under, its values without
	// its cancellation (TriggerSync): what the sync's span starts under,
	// so the span is the request's child.
	ctx context.Context
}

// New builds a daemon from opts. It starts nothing: Run drives the sync
// loop, Handler returns the dashboard, and Serve puts the two together
// on a listener of the daemon's own.
//
// New installs opts.Logger (or slog.Default()) as the destination for
// the in-memory ring buffers as well, so every line the daemon writes
// reaches one place. That call is process-wide — see logger.SetLogger.
func New(opts Options) *Daemon {
	e := opts.Env
	if e == nil {
		e = env.New(opts.Config)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	snitchOK := opts.Snitch
	if snitchOK == nil {
		snitchOK = snitch.OK
	}
	// Only a host's own logger is installed as the ring sink. Passing
	// slog.Default() here instead would work — its handler writes
	// through the log package either way — but it would change the
	// bytes the standalone daemon prints, and the documented default
	// (logger.SetLogger, the README) is the "[label]\tline" form the
	// published command has always written.
	if opts.Logger != nil {
		logger.SetLogger(opts.Logger)
	}

	b := &Daemon{
		conf:       atom.New(opts.Config),
		state:      atom.New(model.New()), // nothing read yet, which the dashboard says
		globalLogs: logger.New("global"),
		env:        e,
		dryrun:     opts.Dryrun,
		log:        log,
		tracer:     opts.Tracer,
		layout:     opts.Layout,
		omitNav:    opts.OmitNav,
		version:    atom.New[int64](0),
		versionCh:  make(chan struct{}, 1),
		syncNow:    make(chan syncRequest, 16),
		history:    history.New(),
		boot:       time.Now(),
		snitch:     snitchOK,
	}
	b.activity = status.New(b.notifyStateChange)
	b.resume = e.Resume
	b.wait = b.waitWake
	e.SetOnProgress(func(sent, total int64) {
		b.meter.progress(sent, total)
		b.activity.Progress(sent, total)
	})
	return b
}

// waitWake blocks until the duration elapses (returning nil, nil), a
// sync-now request arrives (returning the request), or the context is
// cancelled (returning the context's error). Non-positive durations
// return immediately.
func (b *Daemon) waitWake(ctx context.Context, d time.Duration) (*syncRequest, error) {
	if d <= 0 {
		return nil, ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, nil
	case req := <-b.syncNow:
		return &req, nil
	}
}

// TriggerSync requests an immediate sync — a full cycle (all) or a single
// dataset — waking the sync loop if it is idle between cycles. It returns
// false if the request buffer is full.
//
// The sync's span — the cycle's, or the dataset's SyncSpan — starts
// under ctx's values, so a host's tracer parents it to whatever asked
// (POST /api/sync's request). ctx's cancellation is not the sync's: the
// request returns once the sync is queued, and the work is the sync
// loop's, ended by Run's context.
func (b *Daemon) TriggerSync(ctx context.Context, all bool, dataset model.DatasetName) bool {
	select {
	case b.syncNow <- syncRequest{all: all, dataset: dataset, ctx: context.WithoutCancel(ctx)}:
		return true
	default:
		return false
	}
}

// idle waits between sync cycles, tracking the wait (idle after a
// success, backing off after `failures` failed cycles) in the activity
// state. Sync-now requests interrupt it: a global request ends the idle
// immediately, returning the context it was asked under, for the caller
// to start the next cycle under; a per-dataset request syncs just that
// dataset and resumes waiting out the interval. An interval that runs
// out returns no context: the cycle it starts was asked for by nobody.
func (b *Daemon) idle(ctx context.Context, d time.Duration, failures int) (context.Context, error) {
	deadline := time.Now().Add(d)
	remaining := d
	for {
		b.activity.Wait(deadline, failures)
		req, err := b.wait(ctx, remaining)
		if err != nil {
			return nil, err
		}
		if req == nil {
			return nil, nil
		}
		if req.all {
			b.globalLogs.Printf("sync requested; starting cycle now")
			return req.ctx, nil
		}
		b.globalLogs.Printf("sync requested for '%s'", req.dataset)
		if err := b.syncRequested(ctx, req.ctx, req.dataset); err != nil {
			b.globalLogs.Printf("sync error for '%s': %s", req.dataset, err)
			b.activity.FinishDataset(req.dataset, status.QueueFailed)
		} else if b.conf.Deref().PausedFor(req.dataset.Path()) {
			b.activity.FinishDataset(req.dataset, status.QueueSkipped)
		} else {
			b.activity.FinishDataset(req.dataset, status.QueueDone)
		}
		remaining = time.Until(deadline)
	}
}

// syncRequested syncs one dataset outside a cycle, as a SyncSpan whose
// steps nest beneath it — inside a cycle they nest beneath the cycle's.
// The span is the child of the request that asked (startWork). Its
// failure is read with classifyFailure, as each step's is, rather than
// through cycleFailure: that exists to combine a cycle's several
// failures into one verdict and one message, and given one failure it
// reaches classifyFailure's verdict anyway, prefixing the message with
// the dataset this span already carries as an attribute.
func (b *Daemon) syncRequested(ctx, requested context.Context, dataset model.DatasetName) error {
	work, span := b.startWork(ctx, requested, SyncSpan)
	span.Set(DatasetKey, dataset.String())
	err := b.syncDataset(work, dataset)
	endClassified(work, span, err)
	return err
}

// startWork starts the span of a unit of the sync loop's work — a
// cycle, or a dataset synced on request — and returns the context the
// work runs under. Work a sync request asked for starts its span under
// requested, the context it was asked under: its values, the request's
// trace among them, without its cancellation (TriggerSync), so a host's
// tracer parents the span to the request. The work is still the sync
// loop's, though, not the request's, which returned once the sync was
// queued: it runs under ctx — Run's deadline, cancellation and error —
// with the span's values ahead of ctx's (requestedWork). Work nothing
// asked for, a cycle the interval started, has its span start under
// ctx.
func (b *Daemon) startWork(ctx, requested context.Context, name string) (context.Context, Span) {
	if requested == nil {
		return b.startSpan(ctx, name)
	}
	traced, span := b.startSpan(requested, name)
	return requestedWork{Context: ctx, span: traced}, span
}

// requestedWork is the context requested work runs under. When it ends
// is Run's context's to say, so a deadline on Run ends it as it ends a
// cycle nobody asked for, and Run returns that deadline. Its values are
// the span's context's first — what the host's Tracer started under the
// request, which the work's spans nest beneath and a host's outbound
// calls parent to — then Run's, so what a host put on Run's context
// reaches requested work too, the snitch check-in among it.
type requestedWork struct {
	context.Context // Run's: Deadline, Done, Err, and the values span lacks
	span            context.Context
}

func (c requestedWork) Value(key any) any {
	if v := c.span.Value(key); v != nil {
		return v
	}
	return c.Context.Value(key)
}

// event journals a notable daemon-level condition or operator action
// and echoes it to the process log. Steady-state chatter stays on the
// per-call loggers; the journal holds only what an operator returning
// after a week would want to read. The journal carries the dataset as
// a structured field; the process log — the only record that survives
// a restart — gets it as a prefix.
func (b *Daemon) event(level history.Level, dataset *model.DatasetName, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	attrs := []slog.Attr{slog.String(EventKey, string(level))}
	if dataset != nil {
		attrs = append(attrs, slog.String(DatasetKey, dataset.String()))
	}
	b.log.LogAttrs(context.Background(), slogLevel(level), msg, attrs...)
	b.history.RecordEvent(history.Event{At: time.Now(), Level: level, Dataset: dataset, Message: msg})
	b.notifyStateChange()
}

// The names of the spans the daemon records its work as (see Tracer).
// They are exported because a host collects them: "are backups
// running" should be a query for a CycleSpan that ended well, not a
// substring match against a message.
const (
	// CycleSpan is one sync cycle, from its start through the snitch
	// check-in: the refresh, every dataset's sync, the ping.
	CycleSpan = "backupd.cycle"
	// TransferSpan is one transfer to the remote: a transfer step of a
	// plan, or the resume of an interrupted one.
	TransferSpan = "backupd.transfer"
	// DestroySpan is one deletion step of a plan: a snapshot, or a
	// contiguous range of them, at one location.
	DestroySpan = "backupd.destroy"
	// SyncSpan is a single dataset's sync requested outside a cycle.
	SyncSpan = "backupd.sync"
	// SnapshotSpan is a recursive snapshot of the local root taken on
	// request (POST /api/snapshot).
	SnapshotSpan = "backupd.snapshot"
)

// Attribute keys the daemon's spans and records carry. They are
// exported and namespaced for the same reason as the span names.
const (
	// EventKey carries a journal entry's level: info, warning, error.
	EventKey = "backupd.event"
	// DatasetKey carries the dataset a record or span concerns.
	DatasetKey = "backupd.dataset"
	// CyclePausedKey is true on a cycle that ran under a global pause.
	CyclePausedKey = "backupd.cycle.paused"
	// CycleDatasetsKey counts the datasets a cycle processed, and
	// CycleFailuresKey names the ones that failed, present only when
	// some did.
	CycleDatasetsKey = "backupd.cycle.datasets"
	CycleFailuresKey = "backupd.cycle.failures"
	// TransferKindKey says what a transfer sends: TransferInitial,
	// TransferIncremental, TransferFull, or TransferResume.
	TransferKindKey = "backupd.transfer.kind"
	// TransferSentBytesKey is how many bytes went through a transfer's
	// pipe, zero when none moved; TransferBytesPerSecondKey is the rate
	// they moved at: the bytes the pipe moved between its first report
	// of progress and its last, over the time between them — the
	// pipe's own rate, the link's, with neither the send's setup nor
	// the receive's finalization in it — present only when both are
	// nonzero.
	TransferSentBytesKey      = "backupd.transfer.sent_bytes"
	TransferBytesPerSecondKey = "backupd.transfer.bytes_per_second"
	// TransferAbandonedKey is true on a resume that found the
	// interrupted transfer could never finish — its snapshot is gone
	// locally — and aborted it on the remote instead.
	TransferAbandonedKey = "backupd.transfer.abandoned"
	// DestroyLocationKey says where a deletion destroys: local or
	// remote. DestroySnapshotsKey counts the snapshots it destroys.
	DestroyLocationKey  = "backupd.destroy.location"
	DestroySnapshotsKey = "backupd.destroy.snapshots"
	// SnapshotPeriodicityKey is the periodicity a snapshot was taken
	// as: daily, weekly, and so on.
	SnapshotPeriodicityKey = "backupd.snapshot.periodicity"
	// StatusKey and DurationKey are written only by the default tracer,
	// a host's tracer recording outcome and duration its own way:
	// StatusKey is ok, error, or cancelled, and DurationKey how long
	// the span took, in milliseconds.
	StatusKey   = "backupd.status"
	DurationKey = "backupd.duration_ms"
	// SnitchOKKey is false when a Dead Man's Snitch ping failed.
	SnitchOKKey = "backupd.snitch.ok"
)

// OpenTelemetry's names for a failure's text and its class, which a
// failed span or snitch record carries: the error itself, and the
// transport kind when ssh's words say what went wrong (see
// classifyFailure).
const (
	errorMessageKey = "error.message"
	errorTypeKey    = "error.type"
)

// slogLevel maps a journal level onto a slog level, so a host's
// existing level-based routing (alert on warnings and up) works on
// backupd's records without knowing backupd's vocabulary.
func slogLevel(l history.Level) slog.Level {
	switch l {
	case history.Error:
		return slog.LevelError
	case history.Warning:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// applyConfig swaps in a new config. Policy, pause, interval, and snitch
// changes take effect immediately; the settings config.OperatorChanges
// names — the ZFS roots, the SSH endpoint, the escalation prefix, the
// listen address — were captured at startup, so changes there get a
// restart warning.
func (b *Daemon) applyConfig(fresh *config.Config) {
	cur := b.conf.Deref()
	if changed := config.OperatorChanges(cur, fresh); len(changed) > 0 {
		b.event(history.Warning, nil, "%s changed; restart backupd to apply", strings.Join(changed, ", "))
	}
	b.conf.Reset(fresh)
	b.notifyStateChange()
}

// reloadConfigFromDisk picks up hand-edits to the config file at cycle
// start. An invalid or unreadable file keeps the current config.
func (b *Daemon) reloadConfigFromDisk() {
	cur := b.conf.Deref()
	if cur.Path == "" {
		return
	}
	fresh, err := config.LoadFrom(cur.Path)
	if err != nil {
		// Journal only the transition: this runs every cycle, and a
		// typo'd config left on disk must not fill the journal with
		// one warning per cycle.
		if err.Error() != b.lastConfigReloadError {
			b.lastConfigReloadError = err.Error()
			b.event(history.Warning, nil, "config reload failed; keeping current config: %s", err)
		} else {
			b.globalLogs.Printf("config reload failed; keeping current config: %s", err)
		}
		return
	}
	b.lastConfigReloadError = ""
	if bytes.Equal(fresh.Raw, cur.Raw) {
		return
	}
	b.event(history.Info, nil, "reloaded config from %s", cur.Path)
	b.applyConfig(fresh)
}

// targetHas reports whether a snapshot is part of the target inventory at
// a location, tolerating a nil target (no plan generated yet).
func targetHas(target *model.SnapshotInventory, loc model.Location, snap *model.Snapshot) bool {
	if target == nil {
		return false
	}
	switch loc {
	case model.Local:
		return target.Local.Has(snap)
	case model.Remote:
		return target.Remote.Has(snap)
	}
	return false
}

func (b *Daemon) notifyStateChange() {
	b.version.Swap(func(v int64) int64 { return v + 1 })
	select {
	case b.versionCh <- struct{}{}:
	default:
	}
}

// updateStep publishes an update to one step of a dataset's published
// plan. A published model is a value: View, the dashboard and
// /api/state read it after the atom's lock is released, so the update
// is applied to a clone of the dataset — its plan and steps with it —
// and the clone is published in its place.
func (b *Daemon) updateStep(dataset model.DatasetName, stepIndex int, update func(*model.PlanStep)) {
	b.state.Swap(func(state *model.Model) *model.Model {
		currentDS := state.GetDataset(dataset)
		if currentDS == nil || currentDS.Plan == nil || stepIndex >= len(currentDS.Plan.Steps) {
			return state
		}
		updated := currentDS.Clone()
		update(updated.Plan.Steps[stepIndex])
		return model.ReplaceDataset(dataset, updated)(state)
	})
	b.notifyStateChange()
}

// Go runs the sync loop and serves the dashboard on addr — a TCP
// address or a Unix socket's path (see Listen) — until ctx is
// cancelled. It is the standalone daemon; a host that owns its own
// listener runs Run and mounts Handler instead.
func (b *Daemon) Go(ctx context.Context, addr string) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return b.Serve(ctx, addr)
	})

	g.Go(func() error {
		return b.Run(ctx)
	})

	return g.Wait()
}

// Serve runs the dashboard on addr until ctx is cancelled. It does not
// run the sync loop; see Run.
func (b *Daemon) Serve(ctx context.Context, addr string) error {
	return listenAndServe(ctx, addr, b.Handler())
}

// Backoff bounds for failed sync cycles. A failure (e.g. the remote
// refusing SSH) never exits the daemon: the cycle is retried after a delay
// that doubles from minBackoff to maxBackoff, so an extended remote outage
// doesn't hammer the remote while the HTTP server and /snapshot endpoint
// stay available. A successful cycle resets the delay.
const (
	minBackoff = time.Minute
	maxBackoff = 30 * time.Minute
)

// Run drives the sync loop until ctx is cancelled: refresh, plan,
// execute, ping the snitch, wait out the interval, repeat. A failed
// cycle is retried in-process after a growing backoff rather than
// returning, so only cancellation ends it.
func (b *Daemon) Run(ctx context.Context) error {
	backoff := minBackoff
	consecutive := 0
	// requested is the context of the sync request that ended the last
	// wait, which the next cycle's span starts under; nil when the wait
	// ran out, and before the first cycle.
	var requested context.Context
	for {
		c, err := b.cycle(ctx, requested)
		if err != nil {
			return err
		}

		if c.OK {
			backoff = minBackoff
			consecutive = 0
			b.globalLogs.Printf("waiting to restart")
			if requested, err = b.idle(ctx, b.conf.Deref().Interval()-time.Since(c.StartedAt), 0); err != nil {
				return err
			}
		} else {
			consecutive++
			b.globalLogs.Printf("retrying in %s", backoff)
			if requested, err = b.idle(ctx, backoff, consecutive); err != nil {
				return err
			}
			backoff = min(2*backoff, maxBackoff)
		}
	}
}

// cycle runs one sync cycle as a CycleSpan: reload the config, refresh
// every dataset, sync each in turn, record the outcome, and check the
// snitch in after a success. The span is the child of the request that
// asked for the cycle, if one did (startWork), and its context is what
// the steps run under, so their spans nest beneath it and the host's
// check-in parents to it. It returns an error only when ctx ended, and
// then the span ends as that cancellation — the daemon is stopping, and
// a cycle it cut short is no outcome — with no cycle recorded unless
// the cut came during the check-in, after the outcome was in.
//
// A failed cycle Sets error.message to all of its failures and ends
// with the one that decided its level (cycleFailure), so a host that
// classifies the failure where the daemon did not reads that one alone.
func (b *Daemon) cycle(ctx, requested context.Context) (history.Cycle, error) {
	ctx, span := b.startWork(ctx, requested, CycleSpan)
	b.globalLogs.Printf("start")
	b.activity.StartCycle()
	b.reloadConfigFromDisk()
	c := history.Cycle{StartedAt: time.Now(), OK: true}
	err := b.syncAll(ctx, &c)
	c.StoppedAt = time.Now()
	c.Paused = b.conf.Deref().Paused
	span.Set(CycleDatasetsKey, c.Datasets)
	span.Set(CyclePausedKey, c.Paused)
	if len(c.Failures) > 0 {
		names := make([]string, len(c.Failures))
		for i, f := range c.Failures {
			names[i] = f.Dataset.String()
		}
		span.Set(CycleFailuresKey, names)
	}
	if err != nil {
		span.End(slog.LevelInfo, cancellation(ctx))
		return c, err
	}

	b.history.RecordCycle(c)
	b.notifyStateChange()

	if !c.OK {
		level, deciding, kind, message := cycleFailure(c)
		span.Set(errorMessageKey, message)
		if kind != "" {
			span.Set(errorTypeKey, kind)
		}
		span.End(level, errors.New(deciding))
		return c, nil
	}

	conf := b.conf.Deref()
	if conf.SnitchID != "" {
		// A paused system is not a backed-up system: skip the ping so
		// a long-forgotten pause eventually trips the dead man's
		// switch.
		if conf.Paused {
			b.globalLogs.Printf("paused; skipping deadmanssnitch")
		} else if err := b.checkIn(ctx, conf.SnitchID); err != nil {
			span.End(slog.LevelInfo, cancellation(ctx))
			return c, err
		}
	}
	span.End(slog.LevelInfo, nil)
	return c, nil
}

// syncAll refreshes every dataset, then syncs each in turn, recording
// what happened in c: how many datasets it reached, and the refresh
// error or each dataset's failure, either of which fails the cycle. It
// returns an error only when ctx ended.
func (b *Daemon) syncAll(ctx context.Context, c *history.Cycle) error {
	if err := b.refreshAllDatasetsAndPlans(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.OK = false
		c.Error = err.Error()
		b.globalLogs.Printf("error refreshing all datasets and plans: %s", err)
		return nil
	}

	// Then, for each dataset: refresh, replan, resync. The queue lets
	// the UI answer "where in the cycle are we".
	names := b.state.Deref().ListDatasets()
	b.activity.SetQueue(names)
	for _, ds := range names {
		if err := ctx.Err(); err != nil {
			return err
		}

		b.globalLogs.Printf("processing dataset '%s'", ds)
		c.Datasets++

		// Resync with the updated plan
		b.globalLogs.Printf("syncing '%s'", ds)
		if err := b.syncDataset(ctx, ds); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.OK = false
			c.Failures = append(c.Failures, history.DatasetFailure{Dataset: ds, Error: err.Error()})
			err := fmt.Errorf("syncing '%s': %w", ds, err)
			// Log to both global and dataset-specific logs
			b.globalLogs.Printf("sync error; skipping dataset: %s", err)
			b.activity.FinishDataset(ds, status.QueueFailed)
		} else if b.conf.Deref().PausedFor(ds.Path()) {
			b.activity.FinishDataset(ds, status.QueueSkipped)
		} else {
			b.activity.FinishDataset(ds, status.QueueDone)
		}
	}

	b.globalLogs.Printf("synced all datasets")
	return nil
}

// refreshAllDatasetsAndPlans reads both sides from ZFS and publishes
// them, planned. The inventory is rebuilt from ZFS every cycle — state
// that reloads from reality is the point — but each side is published
// only once it has been read whole. Publishing the model as it filled
// showed every dataset with its local side and without its remote for
// the minute thor's remote takes to list over ssh, which the dashboard
// could only read as every dataset never having been replicated.
//
// So the local side goes up when its listing returns, beside the
// remote as it was last read (KeepRemote): unknown after a start, and
// on every cycle after, the previous cycle's inventory, which stands
// until the new listing replaces it. A remote listing that fails
// leaves it standing: an unreachable remote is not an empty one. When
// the remote listing returns, it replaces the remote side of the model
// as published by then, not of the one the cycle began with, so a
// local read that landed meanwhile — a snapshot's — stands. A local
// listing that fails publishes nothing, and the model before it stands.
func (b *Daemon) refreshAllDatasetsAndPlans(ctx context.Context) error {
	local, err := b.readAndPublishLocal(ctx)
	if err != nil {
		return err
	}

	remoteDatasets, err := b.env.RemoteDatasets(b.globalLogs)
	if err != nil {
		return fmt.Errorf("getting remote datasets: %w", err)
	}
	remote := model.New()
	for _, datasetInfo := range remoteDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.RemoteSnapshots(b.globalLogs, datasetInfo.Name)
		if err != nil {
			return fmt.Errorf("getting remote snapshots for '%s': %w", datasetInfo.Name, err)
		}

		remote = model.AddRemoteDataset(datasetInfo.Name, snapshots, datasetInfo.Size)(remote)
	}

	// The model holds what the local listing named, what the remote
	// listing named, and nothing else: a dataset carried only for its
	// remote side, which the remote no longer has, is gone.
	named := append(local.ListDatasets(), remote.ListDatasets()...)
	b.state.Swap(func(cur *model.Model) *model.Model {
		return b.planned(model.Listed(model.Local)(model.ReplaceRemote(remote)(model.Retain(named...)(cur))))
	})
	b.notifyStateChange()
	return nil
}

// readAndPublishLocal lists the local side whole and publishes it
// beside the remote as last read, returning what it read. It holds
// localMu throughout, so no other local read lands between this one
// and its publication.
func (b *Daemon) readAndPublishLocal(ctx context.Context) (*model.Model, error) {
	b.localMu.Lock()
	defer b.localMu.Unlock()

	local := model.New()
	localDatasets, err := b.env.LocalDatasets(b.globalLogs)
	if err != nil {
		return nil, fmt.Errorf("getting local datasets: %s", err)
	}
	for _, datasetInfo := range localDatasets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		snapshots, err := b.env.LocalSnapshots(b.globalLogs, datasetInfo.Name)
		if err != nil {
			return nil, fmt.Errorf("getting snapshots for '%s': %w", datasetInfo.Name, err)
		}

		local = model.AddLocalDataset(datasetInfo.Name, snapshots, datasetInfo.Size)(local)
	}

	b.state.Swap(func(prev *model.Model) *model.Model {
		return b.planned(model.Listed(model.Local)(model.KeepRemote(prev)(local)))
	})
	b.notifyStateChange()
	return local, nil
}

// generatePlansForAllDatasets replans the published model against the
// config in force, so the dashboard shows a config change's effect
// before the next cycle executes it.
func (b *Daemon) generatePlansForAllDatasets() {
	b.state.Swap(b.planned)
}

// planned returns m with each dataset's target and plan computed under
// the current config, for the dashboard: the sync loop replans each
// dataset from a fresh read before executing anything. A dataset with a
// side not yet read gets no plan — against an inventory nobody has
// looked at, the planner would queue a transfer of everything.
func (b *Daemon) planned(m *model.Model) *model.Model {
	conf := b.conf.Deref()
	out := m.Clone()
	for _, name := range out.ListDatasets() {
		ds := out.GetDataset(name)
		if ds.Current == nil || !ds.LocalKnown || !ds.RemoteKnown {
			ds.Target, ds.Plan = nil, nil
			continue
		}
		localPolicy, remotePolicy, keepBaseline := conf.PolicyFor(name.Path())
		target := model.CalculateTargetInventory(ds.Current, localPolicy, remotePolicy, keepBaseline)
		plan, err := model.CalculateTransitionPlan(ds.Current, target)
		if err != nil {
			b.globalLogs.Printf("error generating plan for '%s': %s", name, err)
			continue
		}
		ds.Target, ds.Plan = target, plan
	}
	return out
}

func (b *Daemon) refreshDataset(ctx context.Context, logger *logger.Logger, dataset model.DatasetName) error {
	if err := b.refreshLocal(logger, dataset); err != nil {
		return err
	}

	// Refresh remote snapshots
	remoteSnapshots, err := b.env.RemoteSnapshots(logger, dataset)
	if err != nil {
		if strings.Contains(err.Error(), "dataset does not exist") {
			remoteSnapshots = nil
		} else {
			return fmt.Errorf("getting remote snapshots for '%s': %w", dataset, err)
		}
	}
	b.state.Swap(model.AddRemoteDataset(dataset, remoteSnapshots, nil))

	return nil
}

// refreshLocal reads one dataset's local snapshots and publishes them,
// under localMu so a read that began earlier cannot land over it.
func (b *Daemon) refreshLocal(logger *logger.Logger, dataset model.DatasetName) error {
	b.localMu.Lock()
	defer b.localMu.Unlock()
	localSnapshots, err := b.env.LocalSnapshots(logger, dataset)
	if err != nil {
		return fmt.Errorf("getting local snapshots for '%s': %w", dataset, err)
	}
	b.state.Swap(model.AddLocalDataset(dataset, localSnapshots, nil))
	return nil
}

// remoteParentReady decides whether an initial transfer into dataset
// has a remote parent to be received under. A nested dataset's parent
// is itself a dataset under the same root, replicated ahead of its
// children by the shortest-name-first cycle order, so it is never
// created on the remote — a plain create cannot succeed under an
// encrypted remote whose keys are never loaded, and an empty
// placeholder would collide with the parent's own initial receive.
// "Exists on the remote" is Metrics.HasRemote, from the cycle-start
// enumeration; the snapshot count is what unblocks a child within
// the cycle, once the parent's own transfer step has applied. The
// error says what the dataset is waiting for: a parent with no
// snapshots anywhere is freshly created, and any snapshot of it —
// the next recursive one, or one taken now — ends the wait.
func (b *Daemon) remoteParentReady(dataset model.DatasetName) error {
	parent := path.Dir(dataset.Path())
	if parent == "." || parent == "/" || parent == "" {
		return nil
	}
	ds := b.state.Deref().GetDataset(model.DatasetName(parent))
	if ds == nil || ds.Current == nil {
		// A parent the model doesn't know has no snapshots to wait
		// for; let the receive itself report whatever is wrong.
		return nil
	}
	if ds.Metrics.HasRemote || ds.Current.Remote.Len() > 0 {
		return nil
	}
	if ds.Current.Local.Len() == 0 {
		return fmt.Errorf("waiting for parent dataset '%s' to get its first snapshot: this dataset cannot be received until its parent exists on the remote, and the parent cannot replicate until it has a snapshot — the next recursive snapshot ends the wait, and so does taking one now", parent)
	}
	return fmt.Errorf("waiting for parent dataset '%s' to replicate: this dataset cannot be received until its parent exists on the remote", parent)
}

// syncDataset executes the plan for the given dataset, recording the
// outcome in history (cancellation is not an outcome).
func (b *Daemon) syncDataset(ctx context.Context, dataset model.DatasetName) error {
	err := b.syncDatasetInner(ctx, dataset)
	if err != nil && ctx.Err() == nil {
		b.history.RecordDatasetFailure(dataset, time.Now(), err.Error())
		b.notifyStateChange()
	}
	return err
}

func (b *Daemon) syncDatasetInner(ctx context.Context, dataset model.DatasetName) error {
	b.activity.StartDataset(dataset)

	ds := b.state.Deref().GetDataset(dataset)
	if ds == nil {
		return fmt.Errorf("dataset '%s' not found", dataset)
	}

	// While paused (globally or for this dataset), the state is still
	// refreshed and the plan regenerated — the dashboard keeps showing
	// what would happen — but nothing executes, including resuming an
	// interrupted transfer.
	paused := b.conf.Deref().PausedFor(dataset.Path())

	var resumeErr error
	if !paused {
		// Handle incomplete transfer. A resume failure must not block the
		// deletions in this dataset's plan: they may be exactly what frees
		// the space the resume needs (e.g. a full remote). Hold the error,
		// sync everything up to the plan's first transfer, and return it at
		// the end so the dataset still counts as failed and is retried.
		resumeErr = b.handleIncompleteTransfer(ctx, ds.Logs, dataset)
		if resumeErr != nil {
			resumeErr = fmt.Errorf("handling incomplete transfer of '%s': %w", dataset, resumeErr)
			ds.Logs.Printf("%s; syncing deletions only", resumeErr)
		}
	}

	// Refresh this specific dataset
	if err := b.refreshDataset(ctx, ds.Logs, dataset); err != nil {
		b.globalLogs.Printf("refresh error for '%s': %s", dataset, err)
		return err
	}

	// Re-read the dataset so the plan is generated from the refreshed
	// inventory rather than the cycle-start snapshot of it.
	ds = b.state.Deref().GetDataset(dataset)
	if ds == nil {
		return fmt.Errorf("dataset '%s' not found after refresh", dataset)
	}

	// Snapshots the remote holds that local no longer has, past the
	// last shared one. The plan below discards them — they wedge the
	// dataset otherwise — and each one discarded is journaled by name
	// as it goes: it is the only thing backupd destroys that no
	// retention policy asked it to.
	divergent := model.DivergentRemoteSnapshots(ds.Current)

	// Generate plan
	localPolicy, remotePolicy, keepBaseline := b.conf.Deref().PolicyFor(dataset.Path())
	target := model.CalculateTargetInventory(ds.Current, localPolicy, remotePolicy, keepBaseline)
	plan, err := model.CalculateTransitionPlan(ds.Current, target)
	if err != nil {
		return fmt.Errorf("generating plan for '%s': %w", dataset, err)
	}

	// Publish the plan. The loop keeps its own copy, which TryExecute
	// records each step's status and timing on; the published one is
	// only ever replaced (updateStep).
	b.state.Swap(func(state *model.Model) *model.Model {
		state = state.Clone()
		state.SetPlan(dataset, plan.Clone())
		return state
	})

	// Validate the plan before execution
	if err := model.ValidatePlan(ctx, ds.Current, target, plan, false); err != nil {
		return fmt.Errorf("validating plan for '%s': %w", dataset, err)
	}

	if paused {
		if len(plan.Steps) > 0 {
			ds.Logs.Printf("paused; plan (%d steps) not executed", len(plan.Steps))
		}
		return nil
	}

	for i, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Pause takes effect at step boundaries: the in-flight
		// operation finishes, then nothing new starts. Remaining
		// steps stay pending.
		if b.conf.Deref().PausedFor(dataset.Path()) {
			ds.Logs.Printf("paused; stopping before step %d of %d", i+1, len(plan.Steps))
			return nil
		}

		// A failed resume means the remote still holds partial receive
		// state, so transfers can't proceed; deletions still can.
		if resumeErr != nil && model.IsTransfer(step.Operation) {
			return resumeErr
		}

		// An initial transfer's receive creates only the leaf: until
		// the remote parent exists, this dataset waits, saying so —
		// see remoteParentReady.
		if _, isInitial := step.Operation.(*model.InitialSnapshotTransfer); isInitial {
			if err := b.remoteParentReady(dataset); err != nil {
				return err
			}
		}

		// Get logger from the step's ProcessLogs
		stepLogger := step.Logs
		stepLogger.Printf("Applying op '%s'", step.Operation)
		b.activity.StartStep(i+1, len(plan.Steps), step.Operation.String())

		// The step's span starts here, where it runs, so a step that
		// never ran has none; dry-run runs nothing, and has none either.
		stepCtx := ctx
		var endStep func(error)
		if !b.dryrun {
			stepCtx, endStep = b.startStep(ctx, dataset, step.Operation)
		}

		// TryExecute records the step's status and timing on the
		// loop's copy, and publishes each change.
		err := step.TryExecute(
			func(update func(*model.PlanStep)) {
				b.updateStep(dataset, i, update)
			},
			func() error {
				stepLogger.Printf("-- Ensuring in-memory state supports this update...")
				// Check against the *current* in-memory state, which
				// earlier steps have already advanced — checking the
				// cycle-start state instead would wrongly reject the
				// second of two chained transfers (A→B, then B→C).
				currentDS := b.state.Deref().GetDataset(dataset)
				if currentDS == nil || currentDS.Current == nil {
					return fmt.Errorf("dataset '%s' has no current inventory", dataset)
				}
				_, err := step.Apply(currentDS.Current)
				if err != nil {
					return fmt.Errorf("applying op '%s' to in-memory state of '%s': %w", step, dataset, err)
				}

				// In dryrun mode, we don't actually apply the operations to the ZFS environment
				// We just update the in-memory state for display purposes
				if b.dryrun {
					stepLogger.Printf("-- [DRYRUN] Would update zfs environment with op '%s'", step)
					stepLogger.Printf("-- [DRYRUN] Updating in-memory state only...")
					b.state.Swap(func(state *model.Model) *model.Model {
						currentDS := state.GetDataset(dataset)
						if currentDS == nil || currentDS.Current == nil {
							return state
						}
						newInventory, err := step.Apply(currentDS.Current)
						if err != nil {
							stepLogger.Printf("-- [DRYRUN] Error applying op to current state: %v", err)
							return state
						}
						// Update inventory
						updatedDS := currentDS.Clone()
						updatedDS.Current = newInventory
						return model.ReplaceDataset(dataset, updatedDS)(state)
					})
					b.notifyStateChange()
					stepLogger.Printf("-- [DRYRUN] Done.")
					return nil
				}

				allowRetry := false
				attempts := 0
			retry:
				attempts++

				if err := stepCtx.Err(); err != nil {
					return err
				}

				stepLogger.Printf("-- Updating zfs environment...")
				if err := b.env.Apply(stepCtx, stepLogger, step); err != nil {
					if allowRetry && strings.Contains(err.Error(), "exit status 255") && attempts < 5 {
						stepLogger.Printf("-- Got status code 255 on attempt %d; retrying", attempts)
						time.Sleep(time.Minute * time.Duration(attempts))
						goto retry
					} else {
						return fmt.Errorf("applying op '%s' to zfs env (attempt %d) of '%s': %w", step, attempts, dataset, err)
					}
				}

				stepLogger.Printf("-- Updating in-memory state...")
				b.state.Swap(func(state *model.Model) *model.Model {
					currentDS := state.GetDataset(dataset)
					if currentDS == nil || currentDS.Current == nil {
						return state
					}
					newInventory, err := step.Apply(currentDS.Current)
					if err != nil {
						stepLogger.Printf("-- Error applying op to current state: %v", err)
						return state
					}
					// Update inventory
					updatedDS := currentDS.Clone()
					updatedDS.Current = newInventory
					return model.ReplaceDataset(dataset, updatedDS)(state)
				})
				b.notifyStateChange()

				stepLogger.Printf("-- Done.")
				return nil
			})
		if endStep != nil {
			endStep(err)
		}

		// Record the executed operation in the activity feed. In dryrun
		// mode nothing actually ran, so nothing is recorded.
		if !b.dryrun && ctx.Err() == nil {
			kind := "deletion"
			if model.IsTransfer(step.Operation) {
				kind = "transfer"
			}
			op := history.Op{At: time.Now(), Dataset: dataset, Operation: step.Operation.String(), Kind: kind}
			if step.StoppedAt != nil {
				op.At = *step.StoppedAt
			}
			op.Duration = step.Duration()
			if err != nil {
				op.Error = err.Error()
			}
			b.history.RecordOp(op)

			if err == nil {
				if discarded := divergentDiscards(step.Operation, divergent); len(discarded) > 0 {
					lead := discardedOne
					if len(discarded) > 1 {
						lead = discardedMany
					}
					name := dataset
					b.history.RecordEvent(history.Event{
						At:      time.Now(),
						Level:   history.Warning,
						Dataset: &name,
						Message: lead + strings.Join(discarded, ", ") + discardedWhy,
					})
				}
			}
		}

		if err != nil {
			stepLogger.Printf("-- Error: %s", err)
			// TryExecute has already marked the step failed.
			return err
		}
	}

	if resumeErr == nil {
		b.history.RecordDatasetSuccess(dataset, time.Now())
	}
	return resumeErr
}

// A divergent discard's journal entry names the snapshots,
// comma-separated, after discardedOne or discardedMany, and then says
// why with discardedWhy. The end-to-end tests read the names back out
// by the same words.
const (
	discardedOne  = "discarded remote snapshot "
	discardedMany = "discarded remote snapshots "
	discardedWhy  = ": absent locally and newer than the newest shared snapshot, so the dataset could not transfer at all"
)

// divergentDiscards names every snapshot a step discarded from the
// remote because local no longer had it, and nil for every other
// operation. A range is enumerated rather than reported by its
// endpoints: these are the only deletions no retention policy asked
// for, so the journal owes the operator each name.
//
// Divergent snapshots are exactly the remote's tail past the newest
// shared snapshot, and that snapshot is always in the target, so an
// adjacency group of remote deletions is either wholly divergent or
// not divergent at all.
func divergentDiscards(op model.Operation, divergent *model.Snapshots) []string {
	switch op := op.(type) {
	case *model.SnapshotDeletion:
		if op.Location == model.Remote && divergent.Has(op.Snapshot) {
			return []string{op.Snapshot.Name}
		}
	case *model.SnapshotRangeDeletion:
		if op.Location != model.Remote || !divergent.Has(op.Start) {
			return nil
		}
		var names []string
		for snap := range divergent.All() {
			if !snap.Less(op.Start) && !op.End.Less(snap) {
				names = append(names, snap.Name)
			}
		}
		return names
	}
	return nil
}

func (b *Daemon) handleIncompleteTransfer(ctx context.Context, logger *logger.Logger, dataset model.DatasetName) error {
	ds := b.state.Deref().GetDataset(dataset)
	if ds == nil || ds.Current == nil || ds.Current.Remote == nil {
		return nil
	}

	token, err := b.env.RemoteResumeToken(logger, dataset)
	if err != nil && strings.Contains(err.Error(), "dataset does not exist") {
		return nil
	} else if err != nil {
		return fmt.Errorf("getting resume token for '%s': %w", dataset, err)
	}
	if token == "" {
		return nil
	}

	// If in dryrun mode, skip the actual resume operation but log it
	if b.dryrun {
		logger.Printf("[DRYRUN] Would resume transfer for '%s' with token '%s'", dataset, token)
		return nil
	}

	ctx, span := b.startSpan(ctx, TransferSpan)
	span.Set(DatasetKey, dataset.String())
	span.Set(TransferKindKey, TransferResume)
	b.meter.begin()
	err = b.resumeTransfer(ctx, logger, dataset, token, span)
	b.measureTransfer(span)
	endClassified(ctx, span, err)
	return err
}

// resumeTransfer resumes an interrupted transfer from its token, as the
// resume span: when the remote holds partially-complete state it aborts
// it and tries again, a second pipe in the same span; when the
// transfer can never finish, it aborts it on the remote and marks the
// span abandoned.
func (b *Daemon) resumeTransfer(ctx context.Context, logger *logger.Logger, dataset model.DatasetName, token string, span Span) error {
	start := time.Now()
resume:
	if err := b.resume(ctx, logger, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		logger.Printf("aborting resumable transfer")
		if err := b.env.AbortRemoteResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting resumable on '%s': %w", dataset, err)
		}
		logger.Printf("retrying resume")
		b.meter.nextPipe()
		goto resume
	} else if err != nil && strings.Contains(err.Error(), "no longer exists") {
		// The interrupted send's local snapshot is gone (deleted
		// manually or by a policy change), so the transfer can never
		// resume. Abort it on the remote and plan afresh.
		logger.Printf("transfer cannot resume, aborting it: %s", err)
		if err := b.env.AbortRemoteResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting unresumable transfer on '%s': %w", dataset, err)
		}
		b.history.RecordOp(history.Op{
			At: time.Now(), Dataset: dataset,
			Operation: "abort unresumable transfer",
			Kind:      "resume",
			Duration:  time.Since(start),
		})
		span.Set(TransferAbandonedKey, true)
		return nil
	} else if err != nil {
		if ctx.Err() == nil {
			b.history.RecordOp(history.Op{
				At: time.Now(), Dataset: dataset,
				Operation: "resume interrupted transfer",
				Kind:      "resume",
				Duration:  time.Since(start),
				Error:     err.Error(),
			})
		}
		return fmt.Errorf("resuming transfer on '%s': %w", dataset, err)
	}

	logger.Printf("resume complete")
	b.history.RecordOp(history.Op{
		At: time.Now(), Dataset: dataset,
		Operation: "resume interrupted transfer",
		Kind:      "resume",
		Duration:  time.Since(start),
	})

	return nil
}

func listenAndServe(ctx context.Context, addr string, handler http.Handler) error {
	ln, err := Listen(addr)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	srv := http.Server{Handler: handler}
	errs := make(chan error)
	go func() {
		errs <- srv.Serve(ln)
	}()
	log.Printf("listening at %s", addr)
	select {
	case err := <-errs:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
		cause := context.Cause(ctx)
		shutdownErr := srv.Shutdown(context.Background())
		return errors.Join(cause, shutdownErr)
	}
}

// Debug refreshes one dataset from the environment and writes what the
// daemon would do to it — the diff from current to target, and the plan
// that would get there — without executing anything. It backs the CLI's
// -debug flag.
func (b *Daemon) Debug(ctx context.Context, dataset model.DatasetName, w io.Writer) error {
	if err := b.refreshDataset(ctx, b.globalLogs, dataset); err != nil {
		return err
	}
	return b.Plan(ctx, dataset, w)
}

// Plan writes the plan for the given dataset to w.
func (b *Daemon) Plan(ctx context.Context, dataset model.DatasetName, w io.Writer) error {
	initialState := b.state.Deref()
	ds := initialState.GetDataset(dataset)

	if ds == nil {
		return fmt.Errorf("no such dataset '%s'", dataset)
	}

	if ds.Current == nil {
		return fmt.Errorf("dataset '%s' has no current inventory", dataset)
	}

	localPolicy, remotePolicy, keepBaseline := b.conf.Deref().PolicyFor(dataset.Path())
	target := model.CalculateTargetInventory(ds.Current, localPolicy, remotePolicy, keepBaseline)

	// Store the target in the dataset for display purposes
	updatedDS := ds.Clone()
	updatedDS.Target = target
	b.state.Swap(model.ReplaceDataset(dataset, updatedDS))

	plan, err := model.CalculateTransitionPlan(ds.Current, target)
	if err != nil {
		return fmt.Errorf("constructing plan: %w", err)
	}

	fmt.Fprintln(w, "ACHIEVING CHANGE")
	fmt.Fprint(w, ds.Current.Diff(target))
	fmt.Fprintln(w, "VIA PLAN")
	for _, op := range plan.Steps {
		fmt.Fprintf(w, "- %s\n", op)
	}

	if err := model.ValidatePlan(ctx, ds.Current, target, plan, true); err != nil {
		return fmt.Errorf("invalid plan: %w", err)
	}

	return nil
}

// RefreshLocalSnapshots refreshes local snapshot information for all
// datasets in memory. It takes its turn with the other local reads
// (localMu), waiting out a cycle's local listing — seconds — rather
// than racing it.
func (b *Daemon) RefreshLocalSnapshots(ctx context.Context, logger *logger.Logger) error {
	b.localMu.Lock()
	defer b.localMu.Unlock()
	// Directly update state by refreshing snapshots for all datasets
	// This is concurrency-safe due to the atom's RWMutex
	b.state.Swap(func(currentState *model.Model) *model.Model {
		if currentState == nil {
			return currentState
		}

		newState := currentState
		for _, dsName := range currentState.ListDatasets() {
			snapshots, err := b.env.LocalSnapshots(logger, dsName)
			if err != nil {
				log.Printf("Warning: failed to refresh snapshots for dataset %s: %v", dsName, err)
				continue
			}

			// Get current dataset metrics (preserve existing size info)
			currentDS := currentState.GetDataset(dsName)
			var size *model.DatasetSize
			if currentDS != nil && currentDS.Metrics.HasLocal {
				size = &currentDS.Metrics.LocalSize
			}

			// Update the dataset with new snapshots
			newState = model.AddLocalDataset(dsName, snapshots, size)(newState)
		}

		return newState
	})

	return nil
}

// CallAPI sends a control request to a running daemon at addr — a TCP
// address or a Unix socket's path, as the listen setting takes — and
// logs the response body. The CLI subcommands (snapshot, pause,
// resume, sync) are thin wrappers over this; it needs no daemon of its
// own, only the address of one.
func CallAPI(ctx context.Context, addr, method, path string) error {
	client, base := apiClient(addr, &http.Client{Timeout: 30 * time.Second})

	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned status %d: %s", path, resp.StatusCode, string(body))
	}

	log.Printf("%s", strings.TrimSpace(string(body)))
	return nil
}

// CreateSnapshot asks a running daemon at addr to create a snapshot.
func CreateSnapshot(ctx context.Context, addr, periodicity string) error {
	return CallAPI(ctx, addr, "POST", "/api/snapshot?"+url.Values{"periodicity": {periodicity}}.Encode())
}
