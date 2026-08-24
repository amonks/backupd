package model

import (
	"context"
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// The rapid tests treat the model's pure core — the ordered Snapshots
// collection, target calculation, and transition planning — as a system
// whose invariants must hold for *any* inventory, not just the ones we
// thought of. The generators deliberately produce timestamp collisions
// (a recursive `zfs snapshot` run for daily and weekly in the same
// second yields distinct snapshots with equal creation times) because
// the duplicate-handling paths are the fiddliest part of the code.

var snapshotTypes = []string{"hourly", "daily", "weekly", "monthly"}

// genUniverse generates a set of distinct snapshots for one dataset,
// with creation times drawn from a small range so that same-time
// different-name duplicates occur regularly.
func genUniverse(t *rapid.T) []*Snapshot {
	n := rapid.IntRange(1, 24).Draw(t, "n")
	byID := map[string]*Snapshot{}
	for range n {
		typ := rapid.SampledFrom(snapshotTypes).Draw(t, "type")
		hour := rapid.IntRange(0, 39).Draw(t, "hour")
		snap := &Snapshot{
			Dataset:   "/ds",
			Name:      fmt.Sprintf("%s-t%02d", typ, hour),
			CreatedAt: int64(hour) * 3600,
		}
		byID[snap.ID()] = snap
	}
	var out []*Snapshot
	for _, snap := range byID {
		out = append(out, snap)
	}
	return out
}

// genSubset draws an arbitrary subset of the universe.
func genSubset(t *rapid.T, label string, universe []*Snapshot) []*Snapshot {
	var out []*Snapshot
	for i, snap := range universe {
		if rapid.Bool().Draw(t, fmt.Sprintf("%s-%d", label, i)) {
			out = append(out, snap)
		}
	}
	return out
}

func genPolicy(t *rapid.T, label string) map[string]int {
	policy := map[string]int{}
	for _, typ := range snapshotTypes {
		if rapid.Bool().Draw(t, label+"-has-"+typ) {
			policy[typ] = rapid.IntRange(0, 5).Draw(t, label+"-n-"+typ)
		}
	}
	return policy
}

func idsOf(snaps *Snapshots) map[string]bool {
	out := map[string]bool{}
	for snap := range snaps.All() {
		out[snap.ID()] = true
	}
	return out
}

// TestRapidSnapshotsOrdering: no matter the insertion order, iteration
// is strictly ascending by (CreatedAt, Name), and Oldest/Newest agree
// with it.
func TestRapidSnapshotsOrdering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		perm := rapid.Permutation(universe).Draw(t, "perm")
		snaps := NewSnapshots(perm...)

		if snaps.Len() != len(universe) {
			t.Fatalf("expected %d snapshots, got %d", len(universe), snaps.Len())
		}

		var prev *Snapshot
		count := 0
		for snap := range snaps.All() {
			if prev != nil && !prev.Less(snap) {
				t.Fatalf("iteration out of order: %s before %s", prev.ID(), snap.ID())
			}
			prev = snap
			count++
		}
		if count != len(universe) {
			t.Fatalf("iterated %d snapshots, expected %d", count, len(universe))
		}
		if prev != nil && snaps.Newest().ID() != prev.ID() {
			t.Fatalf("Newest %s != last iterated %s", snaps.Newest().ID(), prev.ID())
		}

		// AllDesc is the exact reverse.
		var desc []*Snapshot
		for snap := range snaps.AllDesc() {
			desc = append(desc, snap)
		}
		if len(desc) > 0 && snaps.Oldest().ID() != desc[len(desc)-1].ID() {
			t.Fatalf("Oldest %s != last of AllDesc %s", snaps.Oldest().ID(), desc[len(desc)-1].ID())
		}
		for i := range desc {
			if j := len(desc) - 1 - i; i < j && !desc[j].Less(desc[i]) {
				t.Fatalf("AllDesc out of order at %d", i)
			}
		}
	})
}

// TestRapidSnapshotsAddDel: a random interleaving of Add and Del
// behaves like a plain set keyed by ID, while keeping order intact.
func TestRapidSnapshotsAddDel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots()
		want := map[string]bool{}

		ops := rapid.IntRange(1, 60).Draw(t, "ops")
		for i := range ops {
			snap := rapid.SampledFrom(universe).Draw(t, fmt.Sprintf("pick-%d", i))
			if rapid.Bool().Draw(t, fmt.Sprintf("add-%d", i)) {
				snaps.Add(snap)
				want[snap.ID()] = true
			} else {
				snaps.Del(snap)
				delete(want, snap.ID())
			}
		}

		got := idsOf(snaps)
		if len(got) != len(want) {
			t.Fatalf("expected %d snapshots, got %d", len(want), len(got))
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("missing %s", id)
			}
		}
		var prev *Snapshot
		for snap := range snaps.All() {
			if prev != nil && !prev.Less(snap) {
				t.Fatalf("order violated after Add/Del: %s before %s", prev.ID(), snap.ID())
			}
			prev = snap
		}
	})
}

// TestRapidSnapshotsAlgebra: Union, Intersection, and Difference match
// their plain-set definitions.
func TestRapidSnapshotsAlgebra(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		a := NewSnapshots(genSubset(t, "a", universe)...)
		b := NewSnapshots(genSubset(t, "b", universe)...)
		aIDs, bIDs := idsOf(a), idsOf(b)

		check := func(name string, got *Snapshots, want func(id string) bool) {
			gotIDs := idsOf(got)
			for _, snap := range universe {
				id := snap.ID()
				if want(id) != gotIDs[id] {
					t.Fatalf("%s: membership of %s: want %v, got %v", name, id, want(id), gotIDs[id])
				}
			}
		}
		check("union", a.Union(b), func(id string) bool { return aIDs[id] || bIDs[id] })
		check("intersection", a.Intersection(b), func(id string) bool { return aIDs[id] && bIDs[id] })
		check("difference", a.Difference(b), func(id string) bool { return aIDs[id] && !bIDs[id] })
	})
}

// TestRapidMatchingPolicy: for each periodicity the policy keeps at most
// the configured count, choosing the newest snapshots of that type.
func TestRapidMatchingPolicy(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots(universe...)
		policy := genPolicy(t, "policy")

		matches := snapshotMatchesByType(snaps.MatchingPolicy(policy))

		// Compute the expected pick independently: the newest N of each
		// type, walking newest-first.
		want := map[string][]string{}
		for snap := range snaps.AllDesc() {
			typ := snap.Type()
			if len(want[typ]) < policy[typ] {
				want[typ] = append(want[typ], snap.ID())
			}
		}

		for _, typ := range snapshotTypes {
			if len(matches[typ]) != len(want[typ]) {
				t.Fatalf("type %s: expected %d matches, got %d", typ, len(want[typ]), len(matches[typ]))
			}
			wantSet := map[string]bool{}
			for _, id := range want[typ] {
				wantSet[id] = true
			}
			for _, id := range matches[typ] {
				if !wantSet[id] {
					t.Fatalf("type %s: unexpected match %s", typ, id)
				}
			}
		}
	})
}

func snapshotMatchesByType(snaps *Snapshots) map[string][]string {
	out := map[string][]string{}
	for snap := range snaps.All() {
		out[snap.Type()] = append(out[snap.Type()], snap.ID())
	}
	return out
}

// TestRapidGroupByAdjacency: the groups partition the subset, and every
// multi-snapshot group is contiguous in the parent collection (so it is
// safe to delete as a `first%last` range).
func TestRapidGroupByAdjacency(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots(universe...)
		subset := NewSnapshots(genSubset(t, "subset", universe)...)

		groups := snaps.GroupByAdjacency(subset)

		// Partition: every subset member is in exactly one group; no
		// group contains anything outside the subset.
		seen := map[string]int{}
		for _, group := range groups {
			for snap := range group.All() {
				if !subset.Has(snap) {
					t.Fatalf("group contains %s, which is not in the subset", snap.ID())
				}
				seen[snap.ID()]++
			}
		}
		for snap := range subset.All() {
			if seen[snap.ID()] != 1 {
				t.Fatalf("subset member %s appears in %d groups", snap.ID(), seen[snap.ID()])
			}
		}

		// Contiguity: each multi-snapshot group's members are adjacent in
		// the parent ordering.
		for _, group := range groups {
			if group.Len() < 2 {
				continue
			}
			inRange := false
			for snap := range snaps.All() {
				if snap.ID() == group.Oldest().ID() {
					inRange = true
				}
				if inRange && !group.Has(snap) {
					t.Fatalf("group %s..%s skips %s in parent order",
						group.Oldest().ID(), group.Newest().ID(), snap.ID())
				}
				if snap.ID() == group.Newest().ID() {
					inRange = false
				}
			}
		}
	})
}

// genInventory builds a current inventory whose remote, when non-empty,
// shares at least one snapshot with local: the remote's contents
// arrived by transfer from local, so a sync point exists. That is the
// only constraint. Remote-only snapshots are generated freely, above
// and below the newest shared one, because the divergent ones are a
// shape the system really produces — backupd transfers a snapshot and
// the snapshot is destroyed locally before the next cycle — and
// excluding them here once hid a wedge that took a dataset out of
// service for two days.
func genInventory(t *rapid.T, universe []*Snapshot) *SnapshotInventory {
	local := NewSnapshots(genSubset(t, "local", universe)...)
	remote := NewSnapshots()
	for i, snap := range universe {
		if local.Has(snap) {
			if rapid.Bool().Draw(t, fmt.Sprintf("shared-%d", i)) {
				remote.Add(snap)
			}
		} else if rapid.Bool().Draw(t, fmt.Sprintf("remoteonly-%d", i)) {
			remote.Add(snap)
		}
	}
	shared := local.Intersection(remote)
	if remote.Len() > 0 && shared.Len() == 0 {
		// Force a shared sync point by copying one local snapshot over,
		// or dropping the remote if local is empty.
		if local.Len() > 0 {
			remote.Add(local.Newest())
		} else {
			remote = NewSnapshots()
		}
	}
	return &SnapshotInventory{Local: local, Remote: remote}
}

// TestRapidPlannerReachesTarget is the core soundness property: for any
// inventory and any policies, the planner produces a valid plan whose
// simulated execution lands exactly on the calculated target, and the
// target itself never invents snapshots or drops the incremental
// transfer base.
func TestRapidPlannerReachesTarget(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		current := genInventory(t, universe)
		localPolicy := genPolicy(t, "local")
		remotePolicy := genPolicy(t, "remote")
		keepBaseline := rapid.Bool().Draw(t, "keepBaseline")

		target := CalculateTargetInventory(current, localPolicy, remotePolicy, keepBaseline)

		// The target only contains snapshots that exist somewhere.
		for snap := range target.Local.All() {
			if !current.Local.Has(snap) {
				t.Fatalf("target.Local invents %s", snap.ID())
			}
		}
		for snap := range target.Remote.All() {
			if !current.Remote.Has(snap) && !current.Local.Has(snap) {
				t.Fatalf("target.Remote invents %s", snap.ID())
			}
		}

		// The newest shared snapshot — the base for in-flight incremental
		// transfers — survives in the target on both sides.
		if base := current.Local.Intersection(current.Remote).Newest(); base != nil {
			if !target.Local.Has(base) || !target.Remote.Has(base) {
				t.Fatalf("target drops the transfer base %s", base.ID())
			}
		}

		plan, err := CalculateTransitionPlan(current, target)
		if err != nil {
			t.Fatalf("planning: %v", err)
		}
		if err := ValidatePlan(context.Background(), current, target, plan, false); err != nil {
			t.Fatalf("plan does not reach target: %v", err)
		}
	})
}

// TestRapidPlannerConverges: repeatedly planning and (simulated)
// executing reaches a fixed point — an empty plan — within a few
// cycles. One extra cycle is expected when keep_baseline=false retires
// an old sync point, but the system must never oscillate.
func TestRapidPlannerConverges(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		universe := genUniverse(t)
		current := genInventory(t, universe)
		localPolicy := genPolicy(t, "local")
		remotePolicy := genPolicy(t, "remote")
		keepBaseline := rapid.Bool().Draw(t, "keepBaseline")

		for cycle := range 4 {
			target := CalculateTargetInventory(current, localPolicy, remotePolicy, keepBaseline)
			plan, err := CalculateTransitionPlan(current, target)
			if err != nil {
				t.Fatalf("cycle %d: planning: %v", cycle, err)
			}
			if len(plan.Steps) == 0 {
				return // converged
			}
			for _, step := range plan.Steps {
				next, err := step.Apply(current)
				if err != nil {
					t.Fatalf("cycle %d: applying %s: %v", cycle, step, err)
				}
				current = next
			}
		}
		t.Fatalf("no convergence after 4 cycles; current %s / %s",
			current.LocalString(), current.RemoteString())
	})
}
