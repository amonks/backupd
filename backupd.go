package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"monks.co/backupd/env"
	"monks.co/backupd/model"
)

type Backupd struct {
	state *model.Model
	env   *env.Env
}

func New() *Backupd {
	return &Backupd{env: env.New()}
}

func (b *Backupd) RefreshState(ctx context.Context) error {
	b.state = model.New()

	localDatasets, err := b.env.Local.GetDatasets()
	if err != nil {
		return err
	}
	for _, dataset := range localDatasets {
		snapshots, err := b.env.Local.GetSnapshots(dataset)
		if err != nil {
			return err
		}

		b.state.AddLocalDataset(dataset, snapshots)
	}

	remoteDatasets, err := b.env.Remote.GetDatasets()
	if err != nil {
		return err
	}
	for _, dataset := range remoteDatasets {
		snapshots, err := b.env.Remote.GetSnapshots(dataset)
		if err != nil {
			return err
		}

		b.state.AddRemoteDataset(dataset, snapshots)
	}

	return nil
}

func (b *Backupd) RefreshDataset(ctx context.Context, dataset model.DatasetName) error {
	if b.state == nil {
		b.state = model.New()
	}

	localSnapshots, err := b.env.Local.GetSnapshots(dataset)
	if err != nil {
		return err
	}
	b.state.AddLocalDataset(dataset, localSnapshots)

	remoteSnapshots, err := b.env.Remote.GetSnapshots(dataset)
	if err != nil {
		return err
	}
	b.state.AddRemoteDataset(dataset, remoteSnapshots)

	return nil
}

// Plan prints the plan for the given dataset
func (b *Backupd) Plan(ctx context.Context, dataset model.DatasetName) error {
	initial := b.state.GetDataset(dataset)

	if initial == nil {
		names := b.state.ListDatasets()
		paths := make([]string, len(names))
		for i, name := range names {
			paths[i] = name.Path()
		}
		return fmt.Errorf("no such dataset '%s'; must be one of {%s}",
			dataset, strings.Join(paths, ", "))
	}

	goal := initial.Goal()
	plan, err := initial.Plan(goal)
	if err != nil {
		return fmt.Errorf("constructing plan: %w", err)
	}
	fmt.Println("FROM LOCAL")
	for snapshot := range initial.Local.All() {
		fmt.Printf("- %s\n", snapshot.Name)
	}
	fmt.Println("FROM REMOTE")
	for snapshot := range initial.Remote.All() {
		fmt.Printf("- %s\n", snapshot.Name)
	}
	fmt.Println("TO REMOTE")
	for snapshot := range goal.Remote.All() {
		fmt.Printf("- %s\n", snapshot.Name)
	}
	fmt.Println("VIA PLAN")
	for _, op := range plan {
		fmt.Printf("- %s\n", op)
	}

	return nil
}

func (b *Backupd) handleIncompleteTransfer(ctx context.Context, dataset model.DatasetName) error {
	if b.state.GetDataset(dataset).Remote == nil {
		return nil
	}

	token, err := b.env.Remote.GetResumeToken(dataset)
	if err != nil && strings.Contains(err.Error(), "dataset does not exist") {
		return nil
	} else if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

resume:
	if err := b.env.Resume(ctx, dataset, token); err != nil && strings.Contains(err.Error(), "contains partially-complete state") {
		if err := b.env.Remote.AbortResumable(dataset); err != nil {
			return err
		}
		goto resume
	} else if err != nil {
		return err
	}

	return nil
}

// Go executes the plan for the given dataset.
func (b *Backupd) Go(ctx context.Context, dataset model.DatasetName) error {
	if err := b.handleIncompleteTransfer(ctx, dataset); err != nil {
		return err
	}

	ds := b.state.GetDataset(dataset)

	goal := ds.Goal()
	plan, err := ds.Plan(goal)
	if err != nil {
		return fmt.Errorf("constructing plan: %w", err)
	}

	if err := ds.ValidatePlan(ctx, goal, plan); err != nil {
		return fmt.Errorf("validating plan: %w", err)
	}

	for _, op := range plan {
		log.Printf("Applying op '%s'", op)

		log.Printf("-- Ensuring in-memory state supports this update...")
		newState, err := op.Apply(b.state.GetDataset(dataset))
		if err != nil {
			return fmt.Errorf("applying op '%s' to in-memory state: %w", op, err)
		}

		attempts := 0
	retry:
		attempts++

		if err := ctx.Err(); err != nil {
			return err
		}

		log.Printf("-- Updating zfs environment...")
		if err := b.env.Apply(ctx, op); err != nil {
			if strings.Contains(err.Error(), "exit status 255") && attempts < 5 {
				log.Printf("-- Got status code 255 on attempt %d; retrying", attempts)
				time.Sleep(time.Minute * time.Duration(attempts))
				goto retry
			} else {
				return fmt.Errorf("applying op '%s' to zfs env (attempt %d): %w", op, attempts, err)
			}
		}

		log.Printf("-- Updating in-memory state...")
		b.state.ReplaceDataset(dataset, newState)

		log.Printf("-- Done.")
	}

	return nil
}
