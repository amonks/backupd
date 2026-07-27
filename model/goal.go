package model

func CalculateTargetInventory(current *SnapshotInventory, localPolicy, remotePolicy map[string]int, keepBaseline bool) *SnapshotInventory {
	localSnapshots := current.Local
	remoteSnapshots := current.Remote

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
