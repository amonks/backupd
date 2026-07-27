package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCycleHistoryRecordsSuccess: a successful cycle lands in the history
// ring with per-dataset last-success timestamps, surviving the model
// reset at the start of the next cycle.
func TestCycleHistoryRecordsSuccess(t *testing.T) {
	local, remote := steadyStateExecutors()
	b := newTestBackupd(testConf(), local, remote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		waits++
		if waits >= 2 {
			cancel()
		}
		return nil, ctx.Err()
	}

	if err := b.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return on cancellation, got: %v", err)
	}

	cycles := b.history.Cycles()
	if len(cycles) != 2 {
		t.Fatalf("expected 2 recorded cycles, got %d", len(cycles))
	}
	for i, c := range cycles {
		if !c.OK {
			t.Errorf("cycle %d: expected OK, got %+v", i, c)
		}
		if c.Datasets != 1 {
			t.Errorf("cycle %d: expected 1 dataset, got %d", i, c.Datasets)
		}
	}
	if _, ok := b.history.LastSuccess("/foo"); !ok {
		t.Error("expected last-success timestamp for /foo")
	}
}

// TestCycleHistoryRecordsFailure: a failed refresh is recorded with the
// error so the dashboard can answer "has this actually been working".
func TestCycleHistoryRecordsFailure(t *testing.T) {
	local, _ := steadyStateExecutors()
	remote := &fakeExecutor{name: "remote", handlers: []fakeHandler{
		{match: "-t filesystem", err: fmt.Errorf("connection refused")},
	}}
	b := newTestBackupd(testConf(), local, remote)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.wait = func(ctx context.Context, d time.Duration) (*syncRequest, error) {
		cancel()
		return nil, ctx.Err()
	}

	if err := b.Sync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Sync to return on cancellation, got: %v", err)
	}

	cycles := b.history.Cycles()
	if len(cycles) != 1 {
		t.Fatalf("expected 1 recorded cycle, got %d", len(cycles))
	}
	if cycles[0].OK {
		t.Error("expected cycle to be recorded as failed")
	}
	if cycles[0].Error == "" {
		t.Error("expected cycle error to be recorded")
	}
	if _, ok := b.history.LastSuccess("/foo"); ok {
		t.Error("expected no last-success timestamp after a failed cycle")
	}
}
