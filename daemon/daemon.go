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
	"strings"
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
	conf       *atom.Atom[*config.Config]
	state      *atom.Atom[*model.Model]
	globalLogs *logger.Logger
	activity   *status.Tracker
	env        env.Interface
	dryrun     bool

	// log is the structured record of what the daemon does — cycle
	// outcomes, journal entries, snitch pings — for a host that
	// collects them. See Options.Logger.
	log *slog.Logger

	// layout and omitNav are how a host wraps the dashboard in its own
	// chrome. See Options.
	layout  Layout
	omitNav bool

	version *atom.Atom[int64]
	versionCh  chan struct{}
	syncNow    chan syncRequest
	history    *history.History
	// boot anchors "since boot" judgments (e.g. how overdue the snitch
	// ping is before any ping has happened).
	boot time.Time

	// lastConfigReloadError edge-guards the reload-failure journal
	// entry; only the sync loop touches it.
	lastConfigReloadError string

	// resume is env.Resume, injectable for tests.
	resume func(context.Context, *logger.Logger, model.DatasetName, string) error

	// snitch pings Dead Man's Snitch, injectable for tests.
	snitch func(id string) error

	// wait blocks for a duration, returning early with a sync-now
	// request if one arrives; injectable for tests.
	wait func(context.Context, time.Duration) (*syncRequest, error)
}

// syncRequest asks the sync loop for an immediate sync: the whole cycle
// (all) or a single dataset.
type syncRequest struct {
	all     bool
	dataset model.DatasetName
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
		state:      atom.New[*model.Model](nil),
		globalLogs: logger.New("global"),
		env:        e,
		dryrun:     opts.Dryrun,
		log:        log,
		layout:     opts.Layout,
		omitNav:    opts.OmitNav,
		version:    atom.New[int64](0),
		versionCh:  make(chan struct{}, 1),
		syncNow:    make(chan syncRequest, 16),
		history:    history.New(),
		boot:       time.Now(),
	}
	b.activity = status.New(b.notifyStateChange)
	b.resume = e.Resume
	b.snitch = snitch.OK
	b.wait = b.waitWake
	e.SetOnProgress(b.activity.Progress)
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
func (b *Daemon) TriggerSync(all bool, dataset model.DatasetName) bool {
	select {
	case b.syncNow <- syncRequest{all: all, dataset: dataset}:
		return true
	default:
		return false
	}
}

// idle waits between sync cycles, tracking the wait (idle after a
// success, backing off after `failures` failed cycles) in the activity
// state. Sync-now requests interrupt it: a global request ends the idle
// immediately (the caller starts the next cycle), while a per-dataset
// request syncs just that dataset and resumes waiting out the interval.
func (b *Daemon) idle(ctx context.Context, d time.Duration, failures int) error {
	deadline := time.Now().Add(d)
	remaining := d
	for {
		b.activity.Wait(deadline, failures)
		req, err := b.wait(ctx, remaining)
		if err != nil {
			return err
		}
		if req == nil {
			return nil
		}
		if req.all {
			b.globalLogs.Printf("sync requested; starting cycle now")
			return nil
		}
		b.globalLogs.Printf("sync requested for '%s'", req.dataset)
		if err := b.syncDataset(ctx, req.dataset); err != nil {
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

// Attribute keys the daemon logs its structured record under. They are
// exported and namespaced because a host collects them: alerting on
// "did the last cycle succeed" should be a query for CycleOKKey, not a
// substring match against a message.
const (
	// EventKey carries a journal entry's level: info, warning, error.
	EventKey = "backupd.event"
	// DatasetKey carries the dataset a record concerns.
	DatasetKey = "backupd.dataset"
	// CycleOKKey is false on a cycle that failed, including one that
	// failed only for some datasets.
	CycleOKKey = "backupd.cycle.ok"
	// CyclePausedKey is true on a cycle that ran under a global pause.
	CyclePausedKey = "backupd.cycle.paused"
	// CycleDatasetsKey counts the datasets a cycle processed, and
	// CycleFailuresKey names the ones that failed.
	CycleDatasetsKey = "backupd.cycle.datasets"
	CycleFailuresKey = "backupd.cycle.failures"
	// CycleDurationKey is how long the cycle took, in milliseconds.
	CycleDurationKey = "backupd.cycle.duration_ms"
	// SnitchOKKey is false when a Dead Man's Snitch ping failed.
	SnitchOKKey = "backupd.snitch.ok"
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
// changes take effect immediately; the ZFS roots and SSH endpoint were
// captured by env at startup, so changes there get a restart warning.
func (b *Daemon) applyConfig(fresh *config.Config) {
	cur := b.conf.Deref()
	if fresh.Local.Root != cur.Local.Root ||
		fresh.Remote.Root != cur.Remote.Root ||
		fresh.Remote.SSHKey != cur.Remote.SSHKey ||
		fresh.Remote.SSHHost != cur.Remote.SSHHost {
		b.event(history.Warning, nil, "local/remote endpoints changed; restart backupd to apply them")
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

// updateStep updates a plan step in a thread-safe manner
func (b *Daemon) updateStep(dataset model.DatasetName, stepIndex int, update func(*model.PlanStep)) {
	b.state.Swap(func(state *model.Model) *model.Model {
		currentDS := state.GetDataset(dataset)
		if currentDS == nil || currentDS.Plan == nil || stepIndex >= len(currentDS.Plan.Steps) {
			return state
		}
		update(currentDS.Plan.Steps[stepIndex])
		return model.ReplaceDataset(dataset, currentDS)(state)
	})
	b.notifyStateChange()
}

// Go runs the sync loop and serves the dashboard on addr until ctx is
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
	for {
		b.globalLogs.Printf("start")
		b.activity.StartCycle()
		b.reloadConfigFromDisk()
		cycleStart := time.Now()
		allOK := true
		var cycleErr string
		var failures []string
		var datasets int

		// At launch: refresh all datasets and generate plans
		if err := b.refreshAllDatasetsAndPlans(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			allOK = false
			cycleErr = err.Error()
			b.globalLogs.Printf("error refreshing all datasets and plans: %s", err)
		} else {
			// Then, for each dataset: refresh, replan, resync. The queue
			// lets the UI answer "where in the cycle are we".
			names := b.state.Deref().ListDatasets()
			b.activity.SetQueue(names)
			for _, ds := range names {
				if err := ctx.Err(); err != nil {
					return err
				}

				b.globalLogs.Printf("processing dataset '%s'", ds)
				datasets++

				// Resync with the updated plan
				b.globalLogs.Printf("syncing '%s'", ds)
				if err := b.syncDataset(ctx, ds); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					allOK = false
					failures = append(failures, ds.String())
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
		}

		cycle := history.Cycle{
			StartedAt: cycleStart,
			StoppedAt: time.Now(),
			OK:        allOK,
			Paused:    b.conf.Deref().Paused,
			Error:     cycleErr,
			Datasets:  datasets,
			Failures:  failures,
		}
		b.history.RecordCycle(cycle)
		b.logCycle(cycle)
		b.notifyStateChange()

		if allOK {
			backoff = minBackoff
			consecutive = 0
			conf := b.conf.Deref()
			if conf.SnitchID != "" {
				// A paused system is not a backed-up system: skip
				// the ping so a long-forgotten pause eventually
				// trips the dead man's switch.
				if conf.Paused {
					b.globalLogs.Printf("paused; skipping deadmanssnitch")
				} else if err := b.snitch(conf.SnitchID); err != nil {
					b.log.LogAttrs(context.Background(), slog.LevelError, "snitch ping failed",
						slog.Bool(SnitchOKKey, false), slog.String("error", err.Error()))
					b.history.RecordSnitchError(time.Now(), err.Error())
				} else {
					b.log.LogAttrs(context.Background(), slog.LevelInfo, "snitch pinged",
						slog.Bool(SnitchOKKey, true))
					b.history.RecordSnitch(time.Now())
				}
			}
			b.globalLogs.Printf("waiting to restart")
			if err := b.idle(ctx, conf.Interval()-time.Since(cycleStart), 0); err != nil {
				return err
			}
		} else {
			consecutive++
			b.globalLogs.Printf("retrying in %s", backoff)
			if err := b.idle(ctx, backoff, consecutive); err != nil {
				return err
			}
			backoff = min(2*backoff, maxBackoff)
		}
	}
}

func (b *Daemon) refreshAllDatasetsAndPlans(ctx context.Context) error {
	b.state.Reset(model.New())

	// First, discover and refresh all datasets
	localDatasets, err := b.env.LocalDatasets(b.globalLogs)
	if err != nil {
		return fmt.Errorf("getting local datasets: %s", err)
	}
	for _, datasetInfo := range localDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.LocalSnapshots(b.globalLogs, datasetInfo.Name)
		if err != nil {
			return fmt.Errorf("getting snapshots for '%s': %w", datasetInfo.Name, err)
		}

		b.state.Swap(model.AddLocalDataset(datasetInfo.Name, snapshots, datasetInfo.Size))
	}

	remoteDatasets, err := b.env.RemoteDatasets(b.globalLogs)
	if err != nil {
		return fmt.Errorf("getting remote datasets: %w", err)
	}
	for _, datasetInfo := range remoteDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.RemoteSnapshots(b.globalLogs, datasetInfo.Name)
		if err != nil {
			return fmt.Errorf("getting remote snapshots for '%s': %w", datasetInfo.Name, err)
		}

		b.state.Swap(model.AddRemoteDataset(datasetInfo.Name, snapshots, datasetInfo.Size))
	}

	// Then generate plans for all datasets to show in UI
	b.generatePlansForAllDatasets(ctx)

	b.notifyStateChange()
	return nil
}

func (b *Daemon) generatePlansForAllDatasets(ctx context.Context) {
	state := b.state.Deref()
	for _, dsName := range state.ListDatasets() {
		if err := ctx.Err(); err != nil {
			return
		}

		ds := state.GetDataset(dsName)
		if ds == nil {
			continue
		}

		// Generate goal and plan for this dataset
		if ds.Current == nil {
			continue
		}
		localPolicy, remotePolicy, keepBaseline := b.conf.Deref().PolicyFor(dsName.Path())
		target := model.CalculateTargetInventory(ds.Current, localPolicy, remotePolicy, keepBaseline)
		plan, err := model.CalculateTransitionPlan(ds.Current, target)
		if err != nil {
			// Log error but continue with other datasets
			b.globalLogs.Printf("error generating plan for '%s': %s", dsName, err)
			continue
		}

		// Update the dataset with goal and plan
		b.state.Swap(func(state *model.Model) *model.Model {
			currentDS := state.GetDataset(dsName)
			if currentDS == nil {
				return state
			}
			updatedDS := currentDS.Clone()
			updatedDS.Target = target
			updatedDS.Plan = plan
			return model.ReplaceDataset(dsName, updatedDS)(state)
		})
	}
}

func (b *Daemon) refreshDataset(ctx context.Context, logger *logger.Logger, dataset model.DatasetName) error {
	// Refresh *local snapshots
	localSnapshots, err := b.env.LocalSnapshots(logger, dataset)
	if err != nil {
		return fmt.Errorf("getting local snapshots for '%s': %w", dataset, err)
	}
	b.state.Swap(model.AddLocalDataset(dataset, localSnapshots, nil))

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

	// Generate plan
	localPolicy, remotePolicy, keepBaseline := b.conf.Deref().PolicyFor(dataset.Path())
	target := model.CalculateTargetInventory(ds.Current, localPolicy, remotePolicy, keepBaseline)
	plan, err := model.CalculateTransitionPlan(ds.Current, target)
	if err != nil {
		return fmt.Errorf("generating plan for '%s': %w", dataset, err)
	}

	// Sync new plan
	b.state.Swap(func(state *model.Model) *model.Model {
		state = state.Clone()
		state.SetPlan(dataset, plan)
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

		// Get logger from the step's ProcessLogs
		stepLogger := step.Logs
		stepLogger.Printf("Applying op '%s'", step.Operation)
		b.activity.StartStep(i+1, len(plan.Steps), step.Operation.String())

		// Use TryExecute to manage status and timing
		err := step.TryExecute(
			func(updateFunc func(*model.PlanStep)) {
				b.updateStep(dataset, i, updateFunc)
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

				if err := ctx.Err(); err != nil {
					return err
				}

				stepLogger.Printf("-- Updating zfs environment...")
				if err := b.env.Apply(ctx, stepLogger, step); err != nil {
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
		}

		if err != nil {
			stepLogger.Printf("-- Error: %s", err)
			// Status is already set to Failed by TryExecute via updateStepStatus
			return err
		}
	}

	if resumeErr == nil {
		b.history.RecordDatasetSuccess(dataset, time.Now())
	}
	return resumeErr
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

	start := time.Now()
resume:
	if err := b.resume(ctx, logger, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		logger.Printf("aborting resumable transfer")
		if err := b.env.AbortRemoteResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting resumable on '%s': %w", dataset, err)
		}
		logger.Printf("retrying resume")
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
	srv := http.Server{Addr: addr, Handler: handler}
	errs := make(chan error)
	go func() {
		errs <- srv.ListenAndServe()
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

// RefreshLocalSnapshots refreshes local snapshot information for all datasets in memory
func (b *Daemon) RefreshLocalSnapshots(ctx context.Context, logger *logger.Logger) error {
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

// CallAPI sends a control request to a running daemon at addr and logs
// the response body. The CLI subcommands (snapshot, pause, resume,
// sync) are thin wrappers over this; it needs no daemon of its own,
// only the address of one.
func CallAPI(ctx context.Context, addr, method, path string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	url := fmt.Sprintf("http://%s%s", addr, path)
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
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
	return CallAPI(ctx, addr, "POST", "/api/snapshot?periodicity="+periodicity)
}

// logCycle emits one structured record per completed cycle: the
// heartbeat a host alerts on. A failing cycle is logged at error level
// so level-based routing catches it without reading the attributes.
func (b *Daemon) logCycle(c history.Cycle) {
	level := slog.LevelInfo
	msg := "sync cycle ok"
	if !c.OK {
		level = slog.LevelError
		msg = "sync cycle failed"
	}
	attrs := []slog.Attr{
		slog.Bool(CycleOKKey, c.OK),
		slog.Bool(CyclePausedKey, c.Paused),
		slog.Int(CycleDatasetsKey, c.Datasets),
		slog.Int64(CycleDurationKey, c.StoppedAt.Sub(c.StartedAt).Milliseconds()),
	}
	if len(c.Failures) > 0 {
		attrs = append(attrs, slog.Any(CycleFailuresKey, c.Failures))
	}
	if c.Error != "" {
		attrs = append(attrs, slog.String("error", c.Error))
	}
	b.log.LogAttrs(context.Background(), level, msg, attrs...)
}
