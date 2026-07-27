package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
	"monks.co/backupd/sync"
)

type Backupd struct {
	conf       *atom.Atom[*config.Config]
	state      *atom.Atom[*model.Model]
	globalLogs *logger.Logger
	syncStatus *sync.Status
	env        *env.Env
	addr       string
	dryrun     bool
	version    *atom.Atom[int64]
	versionCh  chan struct{}
	syncNow    chan syncRequest
	history    *history.History

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

func New(conf *config.Config, addr string, dryrun bool) *Backupd {
	b := &Backupd{
		conf:       atom.New(conf),
		state:      atom.New[*model.Model](nil),
		globalLogs: logger.New("global"),
		syncStatus: sync.New(),
		env:        env.New(conf),
		addr:       addr,
		dryrun:     dryrun,
		version:    atom.New[int64](0),
		versionCh:  make(chan struct{}, 1),
		syncNow:    make(chan syncRequest, 16),
		history:    history.New(),
	}
	b.resume = b.env.Resume
	b.snitch = snitch.OK
	b.wait = b.waitWake
	return b
}

// waitWake blocks until the duration elapses (returning nil, nil), a
// sync-now request arrives (returning the request), or the context is
// cancelled (returning the context's error). Non-positive durations
// return immediately.
func (b *Backupd) waitWake(ctx context.Context, d time.Duration) (*syncRequest, error) {
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
func (b *Backupd) TriggerSync(all bool, dataset model.DatasetName) bool {
	select {
	case b.syncNow <- syncRequest{all: all, dataset: dataset}:
		return true
	default:
		return false
	}
}

// idle waits between sync cycles. Sync-now requests interrupt it: a
// global request ends the idle immediately (the caller starts the next
// cycle), while a per-dataset request syncs just that dataset and
// resumes waiting out the interval.
func (b *Backupd) idle(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	remaining := d
	for {
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
		}
		remaining = time.Until(deadline)
	}
}

// applyConfig swaps in a new config. Policy, pause, interval, and snitch
// changes take effect immediately; the ZFS roots and SSH endpoint were
// captured by env at startup, so changes there get a restart warning.
func (b *Backupd) applyConfig(fresh *config.Config) {
	cur := b.conf.Deref()
	if fresh.Local.Root != cur.Local.Root ||
		fresh.Remote.Root != cur.Remote.Root ||
		fresh.Remote.SSHKey != cur.Remote.SSHKey ||
		fresh.Remote.SSHHost != cur.Remote.SSHHost {
		b.globalLogs.Printf("warning: local/remote endpoints changed; restart backupd to apply them")
	}
	b.conf.Reset(fresh)
	b.notifyStateChange()
}

// reloadConfigFromDisk picks up hand-edits to the config file at cycle
// start. An invalid or unreadable file keeps the current config.
func (b *Backupd) reloadConfigFromDisk() {
	cur := b.conf.Deref()
	if cur.Path == "" {
		return
	}
	fresh, err := config.LoadFrom(cur.Path)
	if err != nil {
		b.globalLogs.Printf("config reload failed; keeping current config: %s", err)
		return
	}
	if bytes.Equal(fresh.Raw, cur.Raw) {
		return
	}
	b.globalLogs.Printf("reloaded config from %s", cur.Path)
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

func (b *Backupd) notifyStateChange() {
	b.version.Swap(func(v int64) int64 { return v + 1 })
	select {
	case b.versionCh <- struct{}{}:
	default:
	}
}

// updateStep updates a plan step in a thread-safe manner
func (b *Backupd) updateStep(dataset model.DatasetName, stepIndex int, update func(*model.PlanStep)) {
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

func (b *Backupd) Go(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return b.Serve(ctx)
	})

	g.Go(func() error {
		return b.Sync(ctx)
	})

	return g.Wait()
}

func (b *Backupd) Serve(ctx context.Context) error {
	return listenAndServe(ctx, b.addr, b.handler())
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

func (b *Backupd) Sync(ctx context.Context) error {
	backoff := minBackoff
	for {
		b.globalLogs.Printf("start")
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
			// Then, for each dataset: refresh, replan, resync
			for _, ds := range b.state.Deref().ListDatasets() {
				if err := ctx.Err(); err != nil {
					return err
				}

				b.globalLogs.Printf("processing dataset '%s'", ds)
				datasets++

				// Resync with the updated plan
				b.globalLogs.Printf("syncing '%s'", ds)
				if err := b.syncDataset(ctx, ds); err != nil {
					allOK = false
					failures = append(failures, ds.String())
					err := fmt.Errorf("syncing '%s': %w", ds, err)
					// Log to both global and dataset-specific logs
					b.globalLogs.Printf("sync error; skipping dataset: %s", err)
					// Also log to dataset-specific location if needed
				}
			}

			b.globalLogs.Printf("synced all datasets")
		}

		b.history.RecordCycle(history.Cycle{
			StartedAt: cycleStart,
			StoppedAt: time.Now(),
			OK:        allOK,
			Paused:    b.conf.Deref().Paused,
			Error:     cycleErr,
			Datasets:  datasets,
			Failures:  failures,
		})
		b.notifyStateChange()

		if allOK {
			backoff = minBackoff
			conf := b.conf.Deref()
			if conf.SnitchID != "" {
				// A paused system is not a backed-up system: skip
				// the ping so a long-forgotten pause eventually
				// trips the dead man's switch.
				if conf.Paused {
					b.globalLogs.Printf("paused; skipping deadmanssnitch")
				} else {
					b.globalLogs.Printf("alerting deadmanssnitch")
					if err := b.snitch(conf.SnitchID); err != nil {
						b.globalLogs.Printf("snitch error: %v", err)
					} else {
						b.globalLogs.Printf("snitched success")
					}
				}
			}
			b.globalLogs.Printf("waiting to restart")
			if err := b.idle(ctx, conf.Interval()-time.Since(cycleStart)); err != nil {
				return err
			}
		} else {
			b.globalLogs.Printf("retrying in %s", backoff)
			if err := b.idle(ctx, backoff); err != nil {
				return err
			}
			backoff = min(2*backoff, maxBackoff)
		}
	}
}

func (b *Backupd) refreshAllDatasetsAndPlans(ctx context.Context) error {
	b.state.Reset(model.New())

	// First, discover and refresh all datasets
	localDatasets, err := b.env.Local.GetDatasets(b.globalLogs)
	if err != nil {
		return fmt.Errorf("getting local datasets: %s", err)
	}
	for _, datasetInfo := range localDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.Local.GetSnapshots(b.globalLogs, datasetInfo.Name)
		if err != nil {
			return fmt.Errorf("getting snapshots for '%s': %w", datasetInfo.Name, err)
		}

		b.state.Swap(model.AddLocalDataset(datasetInfo.Name, snapshots, datasetInfo.Size))
	}

	remoteDatasets, err := b.env.Remote.GetDatasets(b.globalLogs)
	if err != nil {
		return fmt.Errorf("getting remote datasets: %w", err)
	}
	for _, datasetInfo := range remoteDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.Remote.GetSnapshots(b.globalLogs, datasetInfo.Name)
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

func (b *Backupd) generatePlansForAllDatasets(ctx context.Context) {
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

func (b *Backupd) refreshDataset(ctx context.Context, logger *logger.Logger, dataset model.DatasetName) error {
	// Refresh *local snapshots
	localSnapshots, err := b.env.Local.GetSnapshots(logger, dataset)
	if err != nil {
		return fmt.Errorf("getting local snapshots for '%s': %w", dataset, err)
	}
	b.state.Swap(model.AddLocalDataset(dataset, localSnapshots, nil))

	// Refresh remote snapshots
	remoteSnapshots, err := b.env.Remote.GetSnapshots(logger, dataset)
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

// syncDataset executes the plan for the given dataset.
func (b *Backupd) syncDataset(ctx context.Context, dataset model.DatasetName) error {
	// Mark dataset as syncing
	b.syncStatus.SetSyncing(dataset, true)
	defer b.syncStatus.SetSyncing(dataset, false)

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

	// Store initial state for validation during execution
	initialState := b.state.Deref()

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

		// Use TryExecute to manage status and timing
		err := step.TryExecute(
			func(updateFunc func(*model.PlanStep)) {
				b.updateStep(dataset, i, updateFunc)
			},
			func() error {
				stepLogger.Printf("-- Ensuring in-memory state supports this update...")
				initialDS := initialState.GetDataset(dataset)
				if initialDS == nil || initialDS.Current == nil {
					return fmt.Errorf("dataset '%s' has no current inventory", dataset)
				}
				_, err := step.Apply(initialDS.Current)
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

func (b *Backupd) handleIncompleteTransfer(ctx context.Context, logger *logger.Logger, dataset model.DatasetName) error {
	ds := b.state.Deref().GetDataset(dataset)
	if ds == nil || ds.Current == nil || ds.Current.Remote == nil {
		return nil
	}

	token, err := b.env.Remote.GetResumeToken(logger, dataset)
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

resume:
	if err := b.resume(ctx, logger, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		logger.Printf("aborting resumable transfer")
		if err := b.env.Remote.AbortResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting resumable on '%s': %w", dataset, err)
		}
		logger.Printf("retrying resume")
		goto resume
	} else if err != nil && strings.Contains(err.Error(), "no longer exists") {
		// The interrupted send's local snapshot is gone (deleted
		// manually or by a policy change), so the transfer can never
		// resume. Abort it on the remote and plan afresh.
		logger.Printf("transfer cannot resume, aborting it: %s", err)
		if err := b.env.Remote.AbortResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting unresumable transfer on '%s': %w", dataset, err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("resuming transfer on '%s': %w", dataset, err)
	}

	logger.Printf("resume complete")

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

// Plan prints the plan for the given dataset
func (b *Backupd) Plan(ctx context.Context, dataset model.DatasetName) error {
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

	fmt.Println("ACHIEVING CHANGE")
	fmt.Print(ds.Current.Diff(target))
	fmt.Println("VIA PLAN")
	for _, op := range plan.Steps {
		fmt.Printf("- %s\n", op)
	}

	if err := model.ValidatePlan(ctx, ds.Current, target, plan, true); err != nil {
		return fmt.Errorf("invalid plan: %w", err)
	}

	return nil
}

// RefreshLocalSnapshots refreshes local snapshot information for all datasets in memory
func (b *Backupd) RefreshLocalSnapshots(ctx context.Context, logger *logger.Logger) error {
	// Directly update state by refreshing snapshots for all datasets
	// This is concurrency-safe due to the atom's RWMutex
	b.state.Swap(func(currentState *model.Model) *model.Model {
		if currentState == nil {
			return currentState
		}

		newState := currentState
		for _, dsName := range currentState.ListDatasets() {
			snapshots, err := b.env.Local.GetSnapshots(logger, dsName)
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

// CallAPI sends a control request to the running daemon at b.addr and
// logs the response body. The CLI subcommands (snapshot, pause, resume,
// sync) are thin wrappers over this.
func (b *Backupd) CallAPI(ctx context.Context, method, path string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	url := fmt.Sprintf("http://%s%s", b.addr, path)
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

// CreateSnapshot sends a request to the running daemon to create a snapshot
func (b *Backupd) CreateSnapshot(ctx context.Context, periodicity string) error {
	return b.CallAPI(ctx, "POST", "/api/snapshot?periodicity="+periodicity)
}
