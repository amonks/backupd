package model

// DivergentRemoteSnapshots returns the remote snapshots that local no
// longer has and that sort after the newest shared snapshot.
//
// Every incremental send is based on the remote's newest snapshot, so
// one of these at the tip wedges the dataset outright: there is no
// local copy to send from, and the newest shared snapshot — the only
// base the planner has — is by then too old to receive onto. They
// arrive by a race: backupd transfers a snapshot, and the snapshot is
// destroyed locally before the next cycle runs. They cannot be
// re-derived, because local is the source of truth and local no
// longer has them.
//
// So the goal discards them rather than keeping them, and the plan's
// remote deletion is what unwedges the dataset. A replica held
// hostage by a snapshot the source deliberately deleted is the worse
// of the two losses, and nothing else in the system can resolve it —
// left alone, every cycle plans a transfer its own validation
// rejects, forever. The daemon journals each one it discards by
// name: it is the one thing backupd destroys that no retention policy
// asked it to.
//
// Only snapshots after the newest shared one qualify. An older
// remote-only snapshot — an archival baseline the local pool retired
// long ago — blocks nothing, so nothing here condemns it and it stays
// subject to ordinary retention.
//
// With no shared snapshot at all there is no dividing line, so
// nothing is divergent and this function condemns nothing. That is
// total divergence, and it is not resolved here: there is no
// incremental base either, so CalculateTransitionPlan refuses to plan
// transfers into a remote sharing nothing with local. What retention
// does to such a remote is retention's business, unchanged by this.
func DivergentRemoteSnapshots(current *SnapshotInventory) *Snapshots {
	out := NewSnapshots()
	if current == nil || current.Local == nil || current.Remote == nil {
		return out
	}
	base := current.Local.Intersection(current.Remote).Newest()
	if base == nil {
		return out
	}
	for snap := range current.Remote.All() {
		if !current.Local.Has(snap) && base.Less(snap) {
			out.Add(snap)
		}
	}
	return out
}

func CalculateTargetInventory(current *SnapshotInventory, localPolicy, remotePolicy map[string]int, keepBaseline bool) *SnapshotInventory {
	localSnapshots := current.Local
	// Plan against the remote as this plan's own deletions will leave
	// it. A divergent snapshot is already condemned, so it must
	// neither fill a retention slot a live snapshot needs nor stand as
	// the cutoff deciding what is still transferable — counting a
	// snapshot that is about to be destroyed costs a restore point
	// that would otherwise have been kept, and delays the recovery it
	// was blocking by a cycle. It stays in current.Remote, which is
	// what the planner diffs the target against, so its deletion is
	// still derived.
	remoteSnapshots := current.Remote.Difference(DivergentRemoteSnapshots(current))

	sharedSnapshots := localSnapshots.Intersection(remoteSnapshots)
	allSnapshots := localSnapshots.Union(remoteSnapshots)

	goal := &SnapshotInventory{
		Local:  NewSnapshots(),
		Remote: NewSnapshots(),
	}

	// Keep all snapshots matching the policy
	localMatches := allSnapshots.MatchingPolicy(localPolicy)
	for snap := range localMatches.All() {
		// too bad; already lost :shrug:
		if !localSnapshots.Has(snap) {
			continue
		}

		// keep it
		goal.Local.Add(snap)
	}
	remoteMatches := allSnapshots.MatchingPolicy(remotePolicy)
	for snap := range remoteMatches.All() {
		// keep it
		if remoteSnapshots.Has(snap) {
			goal.Remote.Add(snap)
			continue
		}

		// too bad; already lost :shrug:
		if !localSnapshots.Has(snap) {
			continue
		}

		// too bad; already skipped it :shrug: (ordered by the same
		// (CreatedAt, Name) total order used everywhere, so a snapshot
		// sharing its creation second with the remote's newest is only
		// transferable when it sorts after it)
		if newest := remoteSnapshots.Newest(); newest != nil && snap.Less(newest) {
			continue
		}

		// transfer it
		goal.Local.Add(snap)
		goal.Remote.Add(snap)
	}

	if keepBaseline {
		// Keep the oldest snapshot we have
		if snap := localSnapshots.Oldest(); snap != nil {
			goal.Local.Add(snap)
		}
		if snap := remoteSnapshots.Oldest(); snap != nil {
			goal.Remote.Add(snap)
		}

		// Keep the earliest shared snapshot
		if snap := sharedSnapshots.Oldest(); snap != nil {
			goal.Local.Add(snap)
			goal.Remote.Add(snap)
		}
	}

	// Keep the latest shared snapshot: it is the base for incremental
	// transfers, so it survives even with keepBaseline=false
	if snap := sharedSnapshots.Newest(); snap != nil {
		goal.Local.Add(snap)
		goal.Remote.Add(snap)
	}

	return goal
}
