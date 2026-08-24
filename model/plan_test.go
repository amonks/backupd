package model

import (
	"context"
	"testing"
)

// TestPlanRecoversFromDivergentRemoteTip is the /movies regression: for
// two days the planner emitted a transfer based on the newest shared
// snapshot while the remote's tip was a snapshot local no longer had,
// and its own validation rejected the plan every cycle. The plan now
// destroys the tip first, which restores the shared snapshot as the
// base the transfers need.
func TestPlanRecoversFromDivergentRemoteTip(t *testing.T) {
	shared := &Snapshot{Dataset: "/movies", Name: "daily-2026-08-22-01:00:00", CreatedAt: 100}
	orphan := &Snapshot{Dataset: "/movies", Name: "manual-pre-vacuum-2026-08-22", CreatedAt: 200}
	migration := &Snapshot{Dataset: "/movies", Name: "manual-pre-migration-2026-08-22", CreatedAt: 300}
	newest := &Snapshot{Dataset: "/movies", Name: "daily-2026-08-24-01:00:00", CreatedAt: 400}

	current := NewSnapshotInventory(
		NewSnapshots(shared, migration, newest),
		NewSnapshots(shared, orphan),
	)

	localPolicy := map[string]int{"daily": 90, "manual": 1000}
	remotePolicy := map[string]int{"daily": 1, "manual": 1000}

	target := CalculateTargetInventory(current, localPolicy, remotePolicy, true)
	plan, err := CalculateTransitionPlan(current, target)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if err := ValidatePlan(context.Background(), current, target, plan, false); err != nil {
		t.Fatalf("plan does not reach target: %v", err)
	}

	// The deletion comes first: the transfers that follow are only
	// legal once the remote's tip is the shared snapshot again.
	if len(plan.Steps) == 0 {
		t.Fatal("expected a plan")
	}
	del, ok := plan.Steps[0].Operation.(*SnapshotDeletion)
	if !ok || del.Location != Remote || !del.Snapshot.Eq(orphan) {
		t.Fatalf("expected the first step to destroy %s on the remote, got %s", orphan, plan.Steps[0])
	}
	if !target.Remote.Has(migration) || !target.Remote.Has(newest) {
		t.Errorf("expected the blocked snapshots to be transferred once unblocked")
	}
}

// TestPlanKeepsLiveSnapshotDivergentTipWouldDisplace guards the retention
// side of the discard. A divergent snapshot is condemned, so it must not
// occupy the retention slot a live local snapshot needs, nor stand as the
// cutoff deciding what is still transferable: counting it would prune the
// newest local snapshot in favour of one the same plan destroys, leaving
// the dataset with neither.
func TestPlanKeepsLiveSnapshotDivergentTipWouldDisplace(t *testing.T) {
	shared := &Snapshot{Dataset: "/ds", Name: "daily-a", CreatedAt: 1}
	live := &Snapshot{Dataset: "/ds", Name: "daily-b", CreatedAt: 2}
	orphan := &Snapshot{Dataset: "/ds", Name: "daily-c", CreatedAt: 3}

	current := NewSnapshotInventory(
		NewSnapshots(shared, live),
		NewSnapshots(shared, orphan),
	)

	// One daily per side: the orphan is the newest daily anywhere, so
	// an unfiltered policy pass hands it the only slot.
	policy := map[string]int{"daily": 1}
	target := CalculateTargetInventory(current, policy, policy, false)

	if !target.Local.Has(live) {
		t.Errorf("expected the newest live local snapshot to survive retention")
	}
	if !target.Remote.Has(live) {
		t.Errorf("expected the newest live local snapshot to be transferred")
	}
	if target.Remote.Has(orphan) {
		t.Errorf("expected the divergent tip to be dropped from the goal")
	}

	plan, err := CalculateTransitionPlan(current, target)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if err := ValidatePlan(context.Background(), current, target, plan, false); err != nil {
		t.Fatalf("plan does not reach target: %v", err)
	}
	for _, step := range plan.Steps {
		del, ok := step.Operation.(*SnapshotDeletion)
		if ok && del.Location == Local {
			t.Errorf("expected no local deletion, got %s", step)
		}
	}
}
