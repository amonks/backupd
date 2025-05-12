package main

import (
	"context"
	"errors"
	"fmt"
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
	"monks.co/backupd/progress"
	"monks.co/backupd/snitch"
)

type Backupd struct {
	config   *config.Config
	state    *atom.Atom[*model.Model]
	progress *progress.Progress
	env      *env.Env
	addr     string
	dryrun   bool
}

func New(config *config.Config, addr string, dryrun bool) *Backupd {
	return &Backupd{
		config:   config,
		state:    atom.New[*model.Model](nil),
		progress: progress.New(),
		env:      env.New(config),
		addr:     addr,
		dryrun:   dryrun,
	}
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

	// Handle all routes with the generic handler and implement our own routing logic
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// Only handle GET requests
		if req.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		state := b.state.Deref()
		progress := b.progress.Deref()

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
			templ.Handler(index(state, progress, "global", b.dryrun)).ServeHTTP(w, req)
			return
		} else if trimmedPath == "root" {
			// The empty string is used as the dataset name for the root dataset
			// Check if the root dataset exists in the model
			_, ok := state.Datasets[""]
			if !ok {
				http.Error(w, "Root dataset not found", http.StatusNotFound)
				return
			}
			templ.Handler(index(state, progress, "", b.dryrun)).ServeHTTP(w, req)
			return
		}

		// For all other paths, treat them as dataset paths
		// Add leading slash for the dataset model
		datasetForModel := "/" + trimmedPath

		templ.Handler(index(state, progress, datasetForModel, b.dryrun)).ServeHTTP(w, req)
	})

	return listenAndServe(ctx, b.addr, mux)
}

func (b *Backupd) Sync(ctx context.Context) error {
	for {
		b.progress.Log(model.GlobalDataset, "start")
		inAnHour := time.After(time.Hour)
		allOK := true

		if err := b.refreshState(ctx); err != nil {
			return fmt.Errorf("refreshing state: %w", err)
		}

		for _, ds := range b.state.Deref().ListDatasets() {
			if err := ctx.Err(); err != nil {
				return err
			}

			b.progress.Log(model.GlobalDataset, "syncing '%s'", ds)

			if err := b.syncDataset(ctx, ds); err != nil {
				allOK = false
				err := fmt.Errorf("syncing '%s': %w", ds, err)
				// Log to both global and dataset-specific logs
				b.progress.Log(model.GlobalDataset, "sync error; skipping dataset: %s", err)
				b.progress.Log(ds, "sync error: %s", err)
			}
		}

		b.progress.Log(model.GlobalDataset, "synced all datasets")
		if allOK {
			if b.config.SnitchID != "" {
				b.progress.Log(model.GlobalDataset, "alerting deadmanssnitch")
				if err := snitch.OK(b.config.SnitchID); err != nil {
					b.progress.Log(model.GlobalDataset, "snitch error: %v", err)
				} else {
					b.progress.Log(model.GlobalDataset, "snitched success")
				}
			}
			b.progress.Log(model.GlobalDataset, "waiting to restart")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-inAnHour:
			}
		} else {
			b.progress.Log(model.GlobalDataset, "back to top")
		}
	}
}

func (b *Backupd) refreshState(ctx context.Context) error {
	b.state.Reset(model.New())

	logger := b.progress.Logger("refresh-state")
	defer logger.Done()

	localDatasets, err := b.env.Local.GetDatasets(logger)
	if err != nil {
		return fmt.Errorf("getting local datasets: %s", err)
	}
	for _, dataset := range localDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.Local.GetSnapshots(logger, dataset)
		if err != nil {
			return fmt.Errorf("getting snapshots for '%s': %w", dataset, err)
		}

		b.state.Swap(model.AddLocalDataset(dataset, snapshots))
	}

	remoteDatasets, err := b.env.Remote.GetDatasets(logger)
	if err != nil {
		return fmt.Errorf("getting remote datasets: %w", err)
	}
	for _, dataset := range remoteDatasets {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshots, err := b.env.Remote.GetSnapshots(logger, dataset)
		if err != nil {
			return fmt.Errorf("getting remote snapshots for '%s': %w", dataset, err)
		}

		b.state.Swap(model.AddRemoteDataset(dataset, snapshots))
	}

	return nil
}

func (b *Backupd) refreshDataset(ctx context.Context, logger logger.Logger, dataset model.DatasetName) error {
	{
		snapshots, err := b.env.Local.GetSnapshots(logger, dataset)
		if err != nil {
			return fmt.Errorf("getting snapshots for '%s': %w", dataset, err)
		}

		b.state.Swap(model.AddLocalDataset(dataset, snapshots))
	}

	{
		snapshots, err := b.env.Remote.GetSnapshots(logger, dataset)
		if err != nil {
			return fmt.Errorf("getting remote snapshots for '%s': %w", dataset, err)
		}

		b.state.Swap(model.AddRemoteDataset(dataset, snapshots))
	}

	return nil
}

// syncDataset executes the plan for the given dataset.
func (b *Backupd) syncDataset(ctx context.Context, dataset model.DatasetName) error {
	logger := b.progress.Logger(dataset)
	defer logger.Done()

	if err := b.handleIncompleteTransfer(ctx, logger, dataset); err != nil {
		return fmt.Errorf("handling incomplete transfer of '%s': %w", dataset, err)
	}

	initialState := b.state.Deref()
	ds := initialState.GetDataset(dataset)

	goal := ds.Goal(b.config.Local.Policy, b.config.Remote.Policy)
	plan, err := ds.Plan(goal)
	if err != nil {
		return fmt.Errorf("constructing plan for '%s': %w", dataset, err)
	}

	if err := ds.ValidatePlan(ctx, goal, plan, false); err != nil {
		return fmt.Errorf("validating plan for '%s': %w", dataset, err)
	}

	logger.Printf("Plan has %d steps", len(plan))

	for _, op := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}

		logger.Printf("Applying op '%s'", op)

		logger.Printf("-- Ensuring in-memory state supports this update...")
		newState, err := op.Apply(initialState.GetDataset(dataset))
		if err != nil {
			return fmt.Errorf("applying op '%s' to in-memory state of '%s': %w", op, dataset, err)
		}

		// In dryrun mode, we don't actually apply the operations to the ZFS environment
		// We just update the in-memory state for display purposes
		if b.dryrun {
			logger.Printf("-- [DRYRUN] Would update zfs environment with op '%s'", op)
			logger.Printf("-- [DRYRUN] Updating in-memory state only...")
			b.state.Swap(model.ReplaceDataset(dataset, newState))
			logger.Printf("-- [DRYRUN] Done.")
			continue
		}

		allowRetry := false
		attempts := 0
	retry:
		attempts++

		if err := ctx.Err(); err != nil {
			return err
		}

		logger.Printf("-- Updating zfs environment...")
		if err := b.env.Apply(ctx, logger, op); err != nil {
			if allowRetry && strings.Contains(err.Error(), "exit status 255") && attempts < 5 {
				logger.Printf("-- Got status code 255 on attempt %d; retrying", attempts)
				time.Sleep(time.Minute * time.Duration(attempts))
				goto retry
			} else {
				return fmt.Errorf("applying op '%s' to zfs env (attempt %d) of '%s': %w", op, attempts, dataset, err)
			}
		}

		logger.Printf("-- Updating in-memory state...")
		b.state.Swap(model.ReplaceDataset(dataset, newState))

		logger.Printf("-- Done.")
	}

	logger.Printf("sync complete")

	return nil
}

func (b *Backupd) handleIncompleteTransfer(ctx context.Context, logger logger.Logger, dataset model.DatasetName) error {
	if b.state.Deref().GetDataset(dataset).Remote == nil {
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

	goal := ds.Goal(b.config.Local.Policy, b.config.Remote.Policy)
	plan, err := ds.Plan(goal)
	if err != nil {
		return fmt.Errorf("constructing plan: %w", err)
	}
	fmt.Println("ACHIEVING CHANGE")
	fmt.Printf(ds.Diff(goal))
	fmt.Println("VIA PLAN")
	for _, op := range plan {
		fmt.Printf("- %s\n", op)
	}

	if err := ds.ValidatePlan(ctx, goal, plan, true); err != nil {
		return fmt.Errorf("invalid plan: %w", err)
	}

	return nil
}
