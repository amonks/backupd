package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"golang.org/x/sync/errgroup"
	"monks.co/backupd/atom"
	"monks.co/backupd/config"
	"monks.co/backupd/env"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/snitch"
	"monks.co/backupd/sync"
)

type Backupd struct {
	config     *config.Config
	state      *atom.Atom[*model.Model]
	globalLogs *logger.Logger
	syncStatus *sync.Status
	env        *env.Env
	addr       string
	dryrun     bool
	version    *atom.Atom[int64]
	versionCh  chan struct{}
}

func New(config *config.Config, addr string, dryrun bool) *Backupd {
	return &Backupd{
		config:     config,
		state:      atom.New[*model.Model](nil),
		globalLogs: logger.New("global"),
		syncStatus: sync.New(),
		env:        env.New(config),
		addr:       addr,
		dryrun:     dryrun,
		version:    atom.New[int64](0),
		versionCh:  make(chan struct{}, 1),
	}
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
	mux := http.NewServeMux()

	// Handle snapshot creation endpoint
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		periodicity := req.URL.Query().Get("periodicity")
		if periodicity == "" {
			http.Error(w, "Missing periodicity parameter", http.StatusBadRequest)
			return
		}

		root := b.config.Local.Root

		if err := b.env.CreateSnapshotRecursively(ctx, b.globalLogs, root, periodicity); err != nil {
			http.Error(w, fmt.Sprintf("Error creating snapshot: %v", err), http.StatusInternalServerError)
			return
		}

		if err := b.RefreshLocalSnapshots(ctx, b.globalLogs); err != nil {
			http.Error(w, fmt.Sprintf("Error refreshing state: %v", err), http.StatusInternalServerError)
			return
		}

		b.globalLogs.Printf("Created %s snapshot for root %s", periodicity, root)
		b.notifyStateChange()
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Created %s snapshot for root %s\n", periodicity, root)
	})

	// Long-polling endpoint for state changes
	mux.HandleFunc("/poll", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		select {
		case <-b.versionCh:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "refresh")
		case <-time.After(5 * time.Minute):
			w.WriteHeader(http.StatusNoContent)
		case <-req.Context().Done():
			w.WriteHeader(http.StatusNoContent)
		}
	})

	// Handle all routes with the generic handler and implement our own routing logic
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// Only handle GET requests
		if req.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		state := b.state.Deref()
		globalLogs := b.globalLogs.GetLogs()
		syncStatus := b.syncStatus

		// Get the path without the leading slash
		path := req.URL.Path
		if path == "/" {
			// Root path redirects to global view
			http.Redirect(w, req, "/global", http.StatusFound)
			return
		}

		// Remove leading slash for routing but keep it for special cases
		trimmedPath := strings.TrimPrefix(path, "/")

		// Handle special cases first
		if trimmedPath == "global" {
			templ.Handler(index(state, globalLogs, syncStatus, "global", b.dryrun)).ServeHTTP(w, req)
			return
		} else if trimmedPath == "root" {
			// The empty string is used as the dataset name for the root dataset
			// Check if the root dataset exists in the model
			_, ok := state.Datasets[""]
			if !ok {
				http.Error(w, "Root dataset not found", http.StatusNotFound)
				return
			}
			templ.Handler(index(state, globalLogs, syncStatus, "", b.dryrun)).ServeHTTP(w, req)
			return
		}

		// For all other paths, treat them as dataset paths
		// Add leading slash for the dataset model
		datasetForModel := "/" + trimmedPath

		templ.Handler(index(state, globalLogs, syncStatus, datasetForModel, b.dryrun)).ServeHTTP(w, req)
	})

	return listenAndServe(ctx, b.addr, mux)
}

func (b *Backupd) Sync(ctx context.Context) error {
	for {
		b.globalLogs.Printf("start")
		inAnHour := time.After(time.Hour)
		allOK := true

		// At launch: refresh all datasets and generate plans
		if err := b.refreshAllDatasetsAndPlans(ctx); err != nil {
			return fmt.Errorf("refreshing all datasets and plans: %w", err)
		}

		// Then, for each dataset: refresh, replan, resync
		for _, ds := range b.state.Deref().ListDatasets() {
			if err := ctx.Err(); err != nil {
				return err
			}

			b.globalLogs.Printf("processing dataset '%s'", ds)

			// Refresh this specific dataset
			if err := b.refreshDataset(ctx, b.globalLogs, ds); err != nil {
				b.globalLogs.Printf("refresh error for '%s': %s", ds, err)
				continue
			}

			// Replan after refresh
			if err := b.replanDataset(ctx, ds); err != nil {
				b.globalLogs.Printf("replan error for '%s': %s", ds, err)
				continue
			}

			// Resync with the updated plan
			b.globalLogs.Printf("syncing '%s'", ds)
			if err := b.syncDataset(ctx, ds); err != nil {
				allOK = false
				err := fmt.Errorf("syncing '%s': %w", ds, err)
				// Log to both global and dataset-specific logs
				b.globalLogs.Printf("sync error; skipping dataset: %s", err)
				// Also log to dataset-specific location if needed
			}
		}

		b.globalLogs.Printf("synced all datasets")
		if allOK {
			if b.config.SnitchID != "" {
				b.globalLogs.Printf("alerting deadmanssnitch")
				if err := snitch.OK(b.config.SnitchID); err != nil {
					b.globalLogs.Printf("snitch error: %v", err)
				} else {
					b.globalLogs.Printf("snitched success")
				}
			}
			b.globalLogs.Printf("waiting to restart")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-inAnHour:
			}
		} else {
			b.globalLogs.Printf("back to top")
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
		target := model.CalculateTargetInventory(ds.Current, b.config.Local.Policy, b.config.Remote.Policy)
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
		return fmt.Errorf("getting remote snapshots for '%s': %w", dataset, err)
	}
	b.state.Swap(model.AddRemoteDataset(dataset, remoteSnapshots, nil))

	return nil
}

func (b *Backupd) replanDataset(ctx context.Context, dataset model.DatasetName) error {
	ds := b.state.Deref().GetDataset(dataset)
	if ds == nil {
		return fmt.Errorf("dataset '%s' not found", dataset)
	}

	if ds.Current == nil {
		return fmt.Errorf("dataset '%s' has no current inventory", dataset)
	}

	// Generate goal and plan
	target := model.CalculateTargetInventory(ds.Current, b.config.Local.Policy, b.config.Remote.Policy)
	plan, err := model.CalculateTransitionPlan(ds.Current, target)
	if err != nil {
		return fmt.Errorf("generating plan for '%s': %w", dataset, err)
	}

	// Update the dataset with goal and plan
	b.state.Swap(func(state *model.Model) *model.Model {
		currentDS := state.GetDataset(dataset)
		if currentDS == nil {
			return state
		}
		updatedDS := currentDS.Clone()
		updatedDS.Target = target
		updatedDS.Plan = plan
		return model.ReplaceDataset(dataset, updatedDS)(state)
	})

	return nil
}

// syncDataset executes the plan for the given dataset.
func (b *Backupd) syncDataset(ctx context.Context, dataset model.DatasetName) error {
	// Mark dataset as syncing
	b.syncStatus.SetSyncing(dataset, true)
	defer b.syncStatus.SetSyncing(dataset, false)

	// Get the dataset with its pre-generated plan
	ds := b.state.Deref().GetDataset(dataset)
	if ds == nil {
		return fmt.Errorf("dataset '%s' not found", dataset)
	}

	if ds.Plan == nil {
		return fmt.Errorf("no plan available for dataset '%s'", dataset)
	}

	plan := ds.Plan
	target := ds.Target

	if err := b.handleIncompleteTransfer(ctx, plan.Logs, dataset); err != nil {
		return fmt.Errorf("handling incomplete transfer of '%s': %w", dataset, err)
	}

	if target == nil {
		return fmt.Errorf("no target state available for dataset '%s'", dataset)
	}

	// Validate the plan before execution
	if err := model.ValidatePlan(ctx, ds.Current, target, plan, false); err != nil {
		return fmt.Errorf("validating plan for '%s': %w", dataset, err)
	}

	// Store initial state for validation during execution
	initialState := b.state.Deref()

	for i, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return err
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

	return nil
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
	if err := b.env.Resume(ctx, logger, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		logger.Printf("aborting resumable transfer")
		if err := b.env.Remote.AbortResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting resumable on '%s': %w", dataset, err)
		}
		logger.Printf("retrying resume")
		goto resume
	} else if err != nil {
		return fmt.Errorf("resuming transfer on '%s': %w", dataset, err)
	}

	logger.Printf("resume complete")

	if err := b.refreshDataset(ctx, logger, dataset); err != nil {
		return fmt.Errorf("refreshing dataset '%s': %w", dataset, err)
	}

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

	target := model.CalculateTargetInventory(ds.Current, b.config.Local.Policy, b.config.Remote.Policy)

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

// CreateSnapshot sends a request to the running daemon to create a snapshot
func (b *Backupd) CreateSnapshot(ctx context.Context, periodicity string) error {
	client := &http.Client{Timeout: 30 * time.Second}

	url := fmt.Sprintf("http://%s/snapshot?periodicity=%s", b.addr, periodicity)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling snapshot endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("snapshot endpoint returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	log.Printf("Snapshot created: %s", strings.TrimSpace(string(body)))
	return nil
}
