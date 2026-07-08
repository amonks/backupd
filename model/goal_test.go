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
