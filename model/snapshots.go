package model

import (
	"fmt"
	"iter"
	"strings"
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

func (snaps *Snapshots) String() string {
	if snaps == nil {
		return "<no snaps>"
	}
	if snaps.tail == nil || snaps.tail.val == nil {
		return fmt.Sprintf("%d snaps", snaps.Len())
	}
	return fmt.Sprintf("%d → %s", snaps.Len(), snaps.tail.val.Name)
}

func (snaps *Snapshots) Print() string {
	var out strings.Builder
	for _, snap := range snaps.nodes {
		fmt.Fprintf(&out, "  - %s\n", snap.val.ID())
	}
	return out.String()
}

func (snaps *Snapshots) Eq(other *Snapshots) bool {
	if snaps.Len() != other.Len() {
		return false
	}
	rights := other.Each()
	for left := range snaps.Each() {
		right := <-rights
		if left.ID() != right.ID() {
			return false
		}
	}
	return true
}

func (snaps *Snapshots) Each() <-chan *Snapshot {
	out := make(chan *Snapshot)
	go func() {
		for snap := range snaps.All() {
			out <- snap
		}
		close(out)
	}()
	return out
}

func (snaps *Snapshots) Diff(prefix string, other *Snapshots) string {
	removed := snaps.Difference(other)
	added := other.Difference(snaps)

	var out strings.Builder
	for snap := range snaps.Union(other).All() {
		sigil := " "
		if removed.Has(snap) {
			sigil = "-"
		} else if added.Has(snap) {
			sigil = "+"
		}
		fmt.Fprintf(&out, "%s %s %s\n", prefix, sigil, snap.ID())
	}

	return out.String()
}

func (snaps *Snapshots) All() iter.Seq[*Snapshot] {
	return func(yield func(*Snapshot) bool) {
		if snaps == nil {
			return
		}
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
		if snaps == nil {
			return
		}
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
	if snap.Less(snaps.head.val) {
		newNode.next = snaps.head
		snaps.head.prev = newNode
		snaps.head = newNode
		snaps.nodes[snap.ID()] = newNode
		return
	}

	// new tail
	if snap.More(snaps.tail.val) {
		newNode.prev = snaps.tail
		snaps.tail.next = newNode
		snaps.tail = newNode
		snaps.nodes[snap.ID()] = newNode
		return
	}

	// iter to find insertion
	var prev, current = snaps.head, snaps.head.next
	for current != nil && current.val.Less(snap) {
		prev, current = current, current.next
	}

	if current == nil {
		panic("oops")
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
	if snaps == nil {
		return false
	}
	_, exists := snaps.nodes[snap.ID()]
	return exists
}

func (snaps *Snapshots) Len() int {
	if snaps == nil {
		return 0
	}
	return len(snaps.nodes)
}

// Oldest returns the oldest Snapshot.
// It returns nil if there are no snapshots.
func (snaps *Snapshots) Oldest() *Snapshot {
	if snaps == nil {
		return nil
	}
	if snaps.head == nil {
		return nil
	}
	return snaps.head.val
}

// Newest returns the newest Snapshot.
// It returns nil if there are no snapshots.
func (snaps *Snapshots) Newest() *Snapshot {
	if snaps == nil {
		return nil
	}
	if snaps.tail == nil {
		return nil
	}
	return snaps.tail.val
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

func (snaps *Snapshots) GetDuplicates(snap *Snapshot) []*Snapshot {
	var group []*Snapshot
	for candidate := range snaps.All() {
		if candidate.CreatedAt == snap.CreatedAt && candidate.ID() != snap.ID() {
			group = append(group, candidate)
		}
	}
	return group
}

func (snaps *Snapshots) GroupByAdjacency(subset *Snapshots) []*Snapshots {
	if subset.Len() == 0 {
		return nil
	}

	var groups []*Snapshots
	var group *Snapshots

snaploop:
	for candidate := range snaps.All() {
		if subset.Has(candidate) {
			// If this snapshot is a duplicate, and if we don't want to
			// destroy _all_ of its copies, we can't include it in a range.
			// We should:
			// - close the existing group, if any
			// - add this snapshot to a self-closing group
			// - continue
			if dupes := snaps.GetDuplicates(candidate); len(dupes) != 0 {
				for _, dupe := range dupes {
					if !subset.Has(dupe) {
						if group != nil {
							groups = append(groups, group)
							group = nil
						}
						groups = append(groups, NewSnapshots(candidate))
						continue snaploop
					}
				}
			}
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
	if snaps == nil {
		return nil
	}
	out := NewSnapshots()
	for snap := range snaps.All() {
		out.Add(snap)
	}
	return out
}
