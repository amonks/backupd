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
	"monks.co/backupd/env"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
	"monks.co/backupd/progress"
)

type Backupd struct {
	state    *atom.Atom[*model.Model]
	progress *progress.Progress
	env      *env.Env
}

func New() *Backupd {
	return &Backupd{
		state:    atom.New[*model.Model](nil),
		progress: progress.New(),
		env:      env.New(),
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

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		state := b.state.Deref()
		progress := b.progress.Deref()
		templ.Handler(index(state, progress)).ServeHTTP(w, req)
	})

	return listenAndServe(ctx, ":8888", mux)
}

func (b *Backupd) Sync(ctx context.Context) error {
	for {
		if err := b.refreshState(ctx); err != nil {
			return fmt.Errorf("refreshing state: %w", err)
		}

		for _, ds := range b.state.Deref().ListDatasets() {
			if err := ctx.Err(); err != nil {
				return err
			}

			b.progress.Log(model.DatasetName("global"), "syncing '%s'", ds)

			if err := b.syncDataset(ctx, ds); err != nil {
				err := fmt.Errorf("syncing '%s': %w", ds, err)
				b.progress.Log(model.DatasetName("global"), "sync error; skipping dataset: %s", err)
			}
		}

		b.progress.Log(model.DatasetName("global"), "synced all datasets; back to the top")
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

// syncDataset executes the plan for the given dataset.
func (b *Backupd) syncDataset(ctx context.Context, dataset model.DatasetName) error {
	logger := b.progress.Logger(dataset)
	defer logger.Done()

	if err := b.handleIncompleteTransfer(ctx, logger, dataset); err != nil {
		return fmt.Errorf("handling incomplete transfer of '%s': %w", dataset, err)
	}

	initialState := b.state.Deref()
	ds := initialState.GetDataset(dataset)

	goal := ds.Goal()
	plan, err := ds.Plan(goal)
	if err != nil {
		return fmt.Errorf("constructing plan for '%s': %w", dataset, err)
	}

	if err := ds.ValidatePlan(ctx, goal, plan); err != nil {
		return fmt.Errorf("validating plan for '%s': %w", dataset, err)
	}

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

resume:
	if err := b.env.Resume(ctx, logger, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		if err := b.env.Remote.AbortResumable(logger, dataset); err != nil {
			return fmt.Errorf("aborting resumable on '%s': %w", dataset, err)
		}
		goto resume
	} else if err != nil {
		return fmt.Errorf("resuming transfer on '%s': %w", dataset, err)
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
