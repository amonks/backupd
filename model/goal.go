package model

import "log"

func (state *Dataset) Goal(localPolicy, remotePolicy map[string]int) *Dataset {
	localSnapshots := state.Local
	remoteSnapshots := state.Remote

	sharedSnapshots := localSnapshots.Intersection(remoteSnapshots)
	allSnapshots := localSnapshots.Union(remoteSnapshots)

	goal := &Dataset{
		Name:   state.Name,
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

		// too bad; already skipped it :shrug:
		if snap.CreatedAt < remoteSnapshots.Newest().CreatedAt {
			continue
		}

		// transfer it
		log.Printf("keep %s", snap.ID())
		goal.Local.Add(snap)
		goal.Remote.Add(snap)
	}

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

	// Keep the latest shared snapshot
	if snap := sharedSnapshots.Newest(); snap != nil {
		goal.Local.Add(snap)
		goal.Remote.Add(snap)
	}

	return goal
}
