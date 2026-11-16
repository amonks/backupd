package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// The property tests treat the model's pure core — the ordered Snapshots
// collection, target calculation, and transition planning — as a system
// whose invariants must hold for *any* inventory, not just the ones we
// thought of. The generators deliberately produce timestamp collisions
// (a recursive `zfs snapshot` run for daily and weekly in the same
// second yields distinct snapshots with equal creation times) because
// the duplicate-handling paths are the fiddliest part of the code.

var snapshotTypes = []string{"hourly", "daily", "weekly", "monthly"}

// genUniverse generates a set of distinct snapshots for one dataset,
// with creation times drawn from a small range so that same-time
// different-name duplicates occur regularly. The snapshots come back
// in the order first drawn: later draws index into the universe, so
// its order must be a function of the draws for a case to replay.
func genUniverse(tc hegel.TestCase) []*Snapshot {
	n := hegel.Draw(tc, hegel.Integers(1, 24))
	seen := map[string]bool{}
	var out []*Snapshot
	for range n {
		typ := hegel.Draw(tc, hegel.SampledFrom(snapshotTypes))
		hour := hegel.Draw(tc, hegel.Integers(0, 39))
		snap := &Snapshot{
			Dataset:   "/ds",
			Name:      fmt.Sprintf("%s-t%02d", typ, hour),
			CreatedAt: int64(hour) * 3600,
		}
		if !seen[snap.ID()] {
			seen[snap.ID()] = true
			out = append(out, snap)
		}
	}
	return out
}

// genSubset draws an arbitrary subset of the universe.
func genSubset(tc hegel.TestCase, universe []*Snapshot) []*Snapshot {
	var out []*Snapshot
	for _, snap := range universe {
		if hegel.Draw(tc, hegel.Booleans()) {
			out = append(out, snap)
		}
	}
	return out
}

// genPermutation draws an ordering of xs by a Fisher–Yates shuffle,
// which shrinks toward the identity.
func genPermutation[T any](tc hegel.TestCase, xs []T) []T {
	out := slices.Clone(xs)
	for i := range len(out) - 1 {
		j := hegel.Draw(tc, hegel.Integers(i, len(out)-1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func genPolicy(tc hegel.TestCase) map[string]int {
	policy := map[string]int{}
	for _, typ := range snapshotTypes {
		if hegel.Draw(tc, hegel.Booleans()) {
			policy[typ] = hegel.Draw(tc, hegel.Integers(0, 5))
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

// namesOf lists a collection's snapshot names in order, for failure
// messages.
func namesOf(snaps *Snapshots) string {
	var names []string
	for snap := range snaps.All() {
		names = append(names, snap.Name)
	}
	return "[" + strings.Join(names, " ") + "]"
}

// TestSnapshotsOrdering: no matter the insertion order, iteration
// is strictly ascending by (CreatedAt, Name), and Oldest/Newest agree
// with it.
func TestSnapshotsOrdering(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		universe := genUniverse(t)
		perm := genPermutation(t, universe)
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
				t.Fatalf("AllDesc out of order at %d: %s before %s", i, desc[i].ID(), desc[j].ID())
			}
		}
	})
}

// TestSnapshotsAddDel: a random interleaving of Add and Del
// behaves like a plain set keyed by ID, while keeping order intact.
func TestSnapshotsAddDel(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots()
		want := map[string]bool{}

		ops := hegel.Draw(t, hegel.Integers(1, 60))
		for range ops {
			snap := hegel.Draw(t, hegel.SampledFrom(universe))
			if hegel.Draw(t, hegel.Booleans()) {
				snaps.Add(snap)
				want[snap.ID()] = true
			} else {
				snaps.Del(snap)
				delete(want, snap.ID())
			}
		}

		got := idsOf(snaps)
		if len(got) != len(want) {
			t.Fatalf("expected %d snapshots %v, got %d %v", len(want), want, len(got), got)
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

// TestSnapshotsAlgebra: Union, Intersection, and Difference match
// their plain-set definitions.
func TestSnapshotsAlgebra(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		universe := genUniverse(t)
		a := NewSnapshots(genSubset(t, universe)...)
		b := NewSnapshots(genSubset(t, universe)...)
		aIDs, bIDs := idsOf(a), idsOf(b)

		check := func(name string, got *Snapshots, want func(id string) bool) {
			gotIDs := idsOf(got)
			for _, snap := range universe {
				id := snap.ID()
				if want(id) != gotIDs[id] {
					t.Fatalf("%s of %s and %s: membership of %s: want %v, got %v",
						name, namesOf(a), namesOf(b), id, want(id), gotIDs[id])
				}
			}
		}
		check("union", a.Union(b), func(id string) bool { return aIDs[id] || bIDs[id] })
		check("intersection", a.Intersection(b), func(id string) bool { return aIDs[id] && bIDs[id] })
		check("difference", a.Difference(b), func(id string) bool { return aIDs[id] && !bIDs[id] })
	})
}

// TestMatchingPolicy: for each periodicity the policy keeps at most
// the configured count, choosing the newest snapshots of that type.
func TestMatchingPolicy(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots(universe...)
		policy := genPolicy(t)

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
				t.Fatalf("policy %v over %s: type %s: expected %d matches %v, got %d %v",
					policy, namesOf(snaps), typ, len(want[typ]), want[typ], len(matches[typ]), matches[typ])
			}
			wantSet := map[string]bool{}
			for _, id := range want[typ] {
				wantSet[id] = true
			}
			for _, id := range matches[typ] {
				if !wantSet[id] {
					t.Fatalf("policy %v over %s: type %s: unexpected match %s, want %v",
						policy, namesOf(snaps), typ, id, want[typ])
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

// TestGroupByAdjacency: the groups partition the subset, and every
// multi-snapshot group is contiguous in the parent collection (so it is
// safe to delete as a `first%last` range).
func TestGroupByAdjacency(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		universe := genUniverse(t)
		snaps := NewSnapshots(universe...)
		subset := NewSnapshots(genSubset(t, universe)...)

		groups := snaps.GroupByAdjacency(subset)

		// Partition: every subset member is in exactly one group; no
		// group contains anything outside the subset.
		seen := map[string]int{}
		for _, group := range groups {
			for snap := range group.All() {
				if !subset.Has(snap) {
					t.Fatalf("group %s contains %s, which is not in the subset %s",
						namesOf(group), snap.ID(), namesOf(subset))
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
					t.Fatalf("group %s..%s skips %s in parent order %s",
						group.Oldest().ID(), group.Newest().ID(), snap.ID(), namesOf(snaps))
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
func genInventory(tc hegel.TestCase, universe []*Snapshot) *SnapshotInventory {
	local := NewSnapshots(genSubset(tc, universe)...)
	remote := NewSnapshots()
	for _, snap := range universe {
		if local.Has(snap) {
			if hegel.Draw(tc, hegel.Booleans()) {
				remote.Add(snap)
			}
		} else if hegel.Draw(tc, hegel.Booleans()) {
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

// genPlannerCase draws an inventory and the policies applied to it,
// noting them for the counterexample report.
func genPlannerCase(tc hegel.TestCase) (current *SnapshotInventory, localPolicy, remotePolicy map[string]int, keepBaseline bool) {
	universe := genUniverse(tc)
	current = genInventory(tc, universe)
	localPolicy = genPolicy(tc)
	remotePolicy = genPolicy(tc)
	keepBaseline = hegel.Draw(tc, hegel.Booleans())
	tc.Note(fmt.Sprintf("local %s, remote %s; local policy %v, remote policy %v, keep_baseline %v",
		namesOf(current.Local), namesOf(current.Remote), localPolicy, remotePolicy, keepBaseline))
	return current, localPolicy, remotePolicy, keepBaseline
}

// TestPlannerReachesTarget is the core soundness property: for any
// inventory and any policies, the planner produces a valid plan whose
// simulated execution lands exactly on the calculated target, and the
// target itself never invents snapshots or drops the incremental
// transfer base.
func TestPlannerReachesTarget(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		current, localPolicy, remotePolicy, keepBaseline := genPlannerCase(t)

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
				t.Fatalf("target (local %s, remote %s) drops the transfer base %s",
					namesOf(target.Local), namesOf(target.Remote), base.ID())
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

// TestPlannerConverges: repeatedly planning and (simulated)
// executing reaches a fixed point — an empty plan — within a few
// cycles. One extra cycle is expected when keep_baseline=false retires
// an old sync point, but the system must never oscillate.
func TestPlannerConverges(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		current, localPolicy, remotePolicy, keepBaseline := genPlannerCase(t)
		checkConverges(t, current, localPolicy, remotePolicy, keepBaseline)
	})
}

// A recursive snapshot run takes an hourly and a daily snapshot in the
// same second. Sending both to an empty remote is a full send of one
// and an incremental of the other from it, a range that runs in the
// snapshots' (CreatedAt, Name) order since their creation times tie.
func TestPlannerConvergesSendingSameSecondSnapshots(t *testing.T) {
	current := &SnapshotInventory{
		Local: NewSnapshots(
			&Snapshot{Dataset: "/ds", Name: "hourly-t01", CreatedAt: 3600},
			&Snapshot{Dataset: "/ds", Name: "daily-t01", CreatedAt: 3600},
		),
		Remote: NewSnapshots(),
	}
	checkConverges(t, current, map[string]int{}, map[string]int{"hourly": 1, "daily": 1}, false)
}

// checkConverges plans and applies plans from current until one is
// empty, failing t if a plan cannot be made or applied, or if four
// cycles leave work to do.
func checkConverges(t testing.TB, current *SnapshotInventory, localPolicy, remotePolicy map[string]int, keepBaseline bool) {
	t.Helper()
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
	t.Fatalf("no convergence after 4 cycles; current local %s / remote %s",
		namesOf(current.Local), namesOf(current.Remote))
}
