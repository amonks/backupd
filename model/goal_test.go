package model

import "testing"

func TestCalculateTargetInventory_KeepsBaseline(t *testing.T) {
	// The long-standing default: the oldest snapshot at each location and
	// the earliest shared snapshot survive even when no policy matches them.
	oldest := &Snapshot{Name: "weekly-2024", CreatedAt: 1}
	mid := &Snapshot{Name: "daily-1", CreatedAt: 2}
	newest := &Snapshot{Name: "daily-2", CreatedAt: 3}

	current := NewSnapshotInventory(
		NewSnapshots(oldest, mid, newest),
		NewSnapshots(oldest, mid),
	)

	policy := map[string]int{"daily": 1}
	goal := CalculateTargetInventory(current, policy, policy, true)

	if !goal.Local.Has(oldest) {
		t.Errorf("expected oldest local snapshot to be kept as baseline")
	}
	if !goal.Remote.Has(oldest) {
		t.Errorf("expected oldest remote snapshot to be kept as baseline")
	}
	if !goal.Local.Has(mid) || !goal.Remote.Has(mid) {
		t.Errorf("expected latest shared snapshot to be kept on both sides")
	}
	if !goal.Local.Has(newest) || !goal.Remote.Has(newest) {
		t.Errorf("expected policy-matching snapshot to be kept and transferred")
	}
}

func TestCalculateTargetInventory_NoBaseline(t *testing.T) {
	// With keepBaseline=false, only policy matches and the latest shared
	// snapshot (the incremental sync point) survive.
	oldest := &Snapshot{Name: "weekly-2024", CreatedAt: 1}
	mid := &Snapshot{Name: "daily-1", CreatedAt: 2}
	newest := &Snapshot{Name: "daily-2", CreatedAt: 3}

	current := NewSnapshotInventory(
		NewSnapshots(oldest, mid, newest),
		NewSnapshots(oldest, mid),
	)

	policy := map[string]int{"daily": 1}
	goal := CalculateTargetInventory(current, policy, policy, false)

	if goal.Local.Has(oldest) {
		t.Errorf("expected oldest local snapshot to be dropped")
	}
	if goal.Remote.Has(oldest) {
		t.Errorf("expected oldest remote snapshot to be dropped")
	}
	if !goal.Local.Has(mid) || !goal.Remote.Has(mid) {
		t.Errorf("expected latest shared snapshot to be kept as sync point")
	}
	if !goal.Local.Has(newest) || !goal.Remote.Has(newest) {
		t.Errorf("expected policy-matching snapshot to be kept and transferred")
	}
}

func TestCalculateTargetInventory_NoBaselineEmptyPolicy(t *testing.T) {
	// Even with empty policies, the latest shared snapshot is preserved so
	// future incremental transfers have a base.
	oldest := &Snapshot{Name: "weekly-2024", CreatedAt: 1}
	shared := &Snapshot{Name: "daily-1", CreatedAt: 2}

	current := NewSnapshotInventory(
		NewSnapshots(oldest, shared),
		NewSnapshots(oldest, shared),
	)

	goal := CalculateTargetInventory(current, map[string]int{}, map[string]int{}, false)

	if goal.Local.Has(oldest) || goal.Remote.Has(oldest) {
		t.Errorf("expected oldest snapshot to be dropped from both sides")
	}
	if !goal.Local.Has(shared) || !goal.Remote.Has(shared) {
		t.Errorf("expected latest shared snapshot to be kept on both sides")
	}
	if goal.Local.Len() != 1 || goal.Remote.Len() != 1 {
		t.Errorf("expected exactly one snapshot per side, got %dL %dR",
			goal.Local.Len(), goal.Remote.Len())
	}
}

func TestCalculateTargetInventory_DropsDivergentRemoteTip(t *testing.T) {
	// The /movies wedge: backupd transferred a manual snapshot, and the
	// snapshot was destroyed locally before the next cycle ran. The
	// remote's tip is now a snapshot local does not have, so no
	// incremental send has a valid base. The goal drops it — nothing
	// else can unwedge the dataset — while the old remote-only baseline
	// below the sync point is untouched.
	baseline := &Snapshot{Name: "weekly-2023", CreatedAt: 1}
	shared := &Snapshot{Name: "daily-2026-08-22", CreatedAt: 2}
	orphan := &Snapshot{Name: "manual-pre-vacuum", CreatedAt: 3}
	kept := &Snapshot{Name: "manual-pre-migration", CreatedAt: 4}

	current := NewSnapshotInventory(
		NewSnapshots(shared, kept),
		NewSnapshots(baseline, shared, orphan),
	)

	policy := map[string]int{"daily": 1, "manual": 1000, "weekly": 1000}
	goal := CalculateTargetInventory(current, policy, policy, true)

	if goal.Remote.Has(orphan) {
		t.Errorf("expected the divergent remote tip to be dropped from the goal")
	}
	if !goal.Remote.Has(baseline) {
		t.Errorf("expected the remote-only baseline below the sync point to be kept")
	}
	if !goal.Remote.Has(shared) {
		t.Errorf("expected the newest shared snapshot to survive as the transfer base")
	}
	if !goal.Remote.Has(kept) {
		t.Errorf("expected the local snapshot the policy wants to be transferred")
	}
}

func TestDivergentRemoteSnapshots_NoSharedSnapshot(t *testing.T) {
	// Total divergence is not this function's problem to solve:
	// with no shared snapshot there is no dividing line, and
	// CalculateTransitionPlan refuses the whole dataset instead.
	local := &Snapshot{Name: "daily-1", CreatedAt: 1}
	remote := &Snapshot{Name: "daily-2", CreatedAt: 2}

	current := NewSnapshotInventory(NewSnapshots(local), NewSnapshots(remote))

	if got := DivergentRemoteSnapshots(current); got.Len() != 0 {
		t.Errorf("expected no divergent snapshots without a shared base, got %s", got)
	}
}
