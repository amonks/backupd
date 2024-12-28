package model

import (
	"log"
)

func (state *Dataset) Goal() *Dataset {
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
	for snap := range allSnapshots.MatchingPolicy(policy.Local).All() {
		if !localSnapshots.Has(snap) {
			continue
		}
		goal.Local.Add(snap)
	}
	for snap := range allSnapshots.MatchingPolicy(policy.Remote).All() {
		if !localSnapshots.Has(snap) {
			continue
		}
		if remoteSnapshots.Has(snap) {
			goal.Remote.Add(snap)
			continue
		}
		if remoteSnapshots.Len() == 0 {
			goal.Remote.Add(snap)
			continue
		}
		if snap.CreatedAt < remoteSnapshots.Newest().CreatedAt {
			log.Printf("skipped remote snapshot: %s\n", snap)
			continue
		}
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

		// // On remote, delete any unplanned snapshots after the latest shared snapshot
		// var rm []*Snapshot
		// for candidate := range goal.Remote.All() {
		// 	if candidate.CreatedAt > snap.CreatedAt {
		// 		rm = append(rm, candidate)
		// 	}
		// }
		// for _, snap := range rm {
		// 	goal.Remote.Del(snap)
		// }
	}

	return goal
}
