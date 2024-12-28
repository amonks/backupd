package model

import (
	"fmt"
	"iter"
)

type Snapshots struct {
	nodes map[string]*node
	head  *node
	tail  *node
}

type node struct {
	prev *node
	next *node
	val  *Snapshot
}

func NewSnapshots(snapshots ...*Snapshot) *Snapshots {
	snaps := &Snapshots{
		nodes: make(map[string]*node),
	}
	for _, snap := range snapshots {
		snaps.Add(snap)
	}
	return snaps
}

func (snaps *Snapshots) All() iter.Seq[*Snapshot] {
	return func(yield func(*Snapshot) bool) {
		node := snaps.head
		if node == nil {
			return
		}
		for {
			if !yield(node.val) {
				return
			}
			node = node.next
			if node == nil {
				return
			}
		}
	}
}

func (snaps *Snapshots) AllDesc() iter.Seq[*Snapshot] {
	return func(yield func(*Snapshot) bool) {
		node := snaps.tail
		if node == nil {
			return
		}
		for {
			if !yield(node.val) {
				return
			}
			node = node.prev
			if node == nil {
				return
			}
		}
	}
}

func (snaps *Snapshots) Add(snap *Snapshot) {
	// already added
	if _, has := snaps.nodes[snap.ID()]; has {
		return
	}

	newNode := &node{
		val: snap,
	}

	// new head and tail (was empty)
	if snaps.head == nil {
		snaps.head = newNode
		snaps.tail = newNode
		snaps.nodes[snap.ID()] = newNode
		return
	}

	// new head
	if snap.CreatedAt < snaps.head.val.CreatedAt {
		newNode.next = snaps.head
		snaps.head.prev = newNode
		snaps.head = newNode
		snaps.nodes[snap.ID()] = newNode
		return
	}

	// new tail
	if snap.CreatedAt > snaps.tail.val.CreatedAt {
		newNode.prev = snaps.tail
		snaps.tail.next = newNode
		snaps.tail = newNode
		snaps.nodes[snap.ID()] = newNode
		return
	}

	// iter to find insertion
	var prev, current = snaps.head, snaps.head.next
	for current != nil && current.val.CreatedAt < snap.CreatedAt {
		prev, current = current, current.next
	}

	if current == nil {
		return // Should not happen, handled by previous conditions
	}

	newNode.next = current
	newNode.prev = prev
	prev.next = newNode
	current.prev = newNode
	snaps.nodes[snap.ID()] = newNode
}

func (snaps *Snapshots) Del(snap *Snapshot) {
	id := snap.ID()

	node, hasNode := snaps.nodes[id]
	if !hasNode {
		return
	}

	// Update head or tail if necessary
	if node == snaps.head {
		snaps.head = node.next
	}
	if node == snaps.tail {
		snaps.tail = node.prev
	}

	// Relink prev and next if they're not nil
	if node.prev != nil {
		node.prev.next = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	}

	// Remove from map and clean up
	delete(snaps.nodes, id)
	node.prev = nil
	node.next = nil
	node.val = nil
}

func (snaps *Snapshots) Has(snap *Snapshot) bool {
	_, exists := snaps.nodes[snap.ID()]
	return exists
}

func (snaps *Snapshots) Len() int {
	return len(snaps.nodes)
}

// Oldest returns the oldest Snapshot.
// It returns nil if there are no snapshots.
func (snaps *Snapshots) Oldest() *Snapshot {
	if snaps.head == nil {
		return nil
	}
	return snaps.head.val
}

// Newest returns the newest Snapshot.
// It returns nil if there are no snapshots.
func (snaps *Snapshots) Newest() *Snapshot {
	if snaps.tail == nil {
		return nil
	}
	return snaps.tail.val
}

func (snaps *Snapshots) Print() {
	for snap := range snaps.All() {
		fmt.Println(snap)
	}
}

func (snapshots *Snapshots) MatchingPolicy(policy map[string]int) *Snapshots {
	matches := NewSnapshots()
	accum := map[string]int{}
	for snapshot := range snapshots.AllDesc() {
		typ := snapshot.Type()
		if target, hasPolicy := policy[typ]; hasPolicy && accum[typ] < target {
			accum[typ]++
			matches.Add(snapshot)
		}
	}
	return matches
}

func (snaps *Snapshots) Union(other *Snapshots) *Snapshots {
	union := NewSnapshots()
	for snap := range snaps.All() {
		union.Add(snap)
	}
	for snap := range other.All() {
		union.Add(snap)
	}
	return union
}

func (snaps *Snapshots) Intersection(other *Snapshots) *Snapshots {
	intersection := NewSnapshots()
	for snap := range snaps.All() {
		if other.Has(snap) {
			intersection.Add(snap)
		}
	}
	return intersection
}

func (snaps *Snapshots) Difference(other *Snapshots) *Snapshots {
	difference := NewSnapshots()
	for snap := range snaps.All() {
		if !other.Has(snap) {
			difference.Add(snap)
		}
	}
	return difference
}

func (snaps *Snapshots) GroupByAdjacency(subset *Snapshots) []*Snapshots {
	var groups []*Snapshots
	var group *Snapshots
	for candidate := range snaps.All() {
		if subset.Has(candidate) {
			if group == nil {
				group = NewSnapshots(candidate)
			} else {
				group.Add(candidate)
			}
		} else {
			if group != nil {
				groups = append(groups, group)
				group = nil
			}
		}
	}
	if group != nil {
		groups = append(groups, group)
	}
	return groups
}

func (snaps *Snapshots) Clone() *Snapshots {
	out := NewSnapshots()
	for snap := range snaps.All() {
		out.Add(snap)
	}
	return out
}
