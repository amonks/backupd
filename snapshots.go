// Write four functions:
// - OldestLocal
// - NewestLocal
// - OldestRemote
// - NewestRemote

package main

import (
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
	return snaps.iterPred(func(*Snapshot) bool { return true })
}

func (snaps *Snapshots) Local() iter.Seq[*Snapshot] {
	return snaps.iterPred(func(snap *Snapshot) bool { return IsTrue(snap.IsOnLocal) })
}

func (snaps *Snapshots) Remote() iter.Seq[*Snapshot] {
	return snaps.iterPred(func(snap *Snapshot) bool { return IsTrue(snap.IsOnRemote) })
}

func (snaps *Snapshots) AllDesc() iter.Seq[*Snapshot] {
	return snaps.iterPredDesc(func(*Snapshot) bool { return true })
}

func (snaps *Snapshots) LocalDesc() iter.Seq[*Snapshot] {
	return snaps.iterPredDesc(func(snap *Snapshot) bool { return IsTrue(snap.IsOnLocal) })
}

func (snaps *Snapshots) RemoteDesc() iter.Seq[*Snapshot] {
	return snaps.iterPredDesc(func(snap *Snapshot) bool { return IsTrue(snap.IsOnRemote) })
}

func (snaps *Snapshots) iterPred(pred func(snap *Snapshot) bool) iter.Seq[*Snapshot] {
	return func(yield func(*Snapshot) bool) {
		node := snaps.head
		for {
			if pred(node.val) {
				if !yield(node.val) {
					return
				}
			}
			node = node.next
			if node == nil {
				return
			}
		}
	}
}
func (snaps *Snapshots) iterPredDesc(pred func(snap *Snapshot) bool) iter.Seq[*Snapshot] {
	return func(yield func(*Snapshot) bool) {
		node := snaps.tail
		for {
			if pred(node.val) {
				if !yield(node.val) {
					return
				}
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
	if _, has := snaps.nodes[snap.Name]; has {
		return
	}

	newNode := &node{
		val: snap,
	}

	// new head and tail (was empty)
	if snaps.head == nil {
		snaps.head = newNode
		snaps.tail = newNode
		snaps.nodes[snap.Name] = newNode
		return
	}

	// new head
	if snap.CreatedAt < snaps.head.val.CreatedAt {
		newNode.next = snaps.head
		snaps.head.prev = newNode
		snaps.head = newNode
		snaps.nodes[snap.Name] = newNode
		return
	}

	// new tail
	if snap.CreatedAt > snaps.tail.val.CreatedAt {
		newNode.prev = snaps.tail
		snaps.tail.next = newNode
		snaps.tail = newNode
		snaps.nodes[snap.Name] = newNode
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
	snaps.nodes[snap.Name] = newNode
}

func (snaps *Snapshots) Del(name string) {
	node, hasNode := snaps.nodes[name]
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
	delete(snaps.nodes, name)
	node.prev = nil
	node.next = nil
	node.val = nil
}

func (snaps *Snapshots) Has(snap *Snapshot) bool {
	_, exists := snaps.nodes[snap.Name]
	return exists
}

func (snaps *Snapshots) Len() int {
	return len(snaps.nodes)
}

func (snaps *Snapshots) LenLocal() int {
	count := 0
	for range snaps.Local() {
		count++
	}
	return count
}

func (snaps *Snapshots) LenRemote() int {
	count := 0
	for range snaps.Remote() {
		count++
	}
	return count
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

// OldestLocal returns the oldest local Snapshot.
// It returns nil if there are no local snapshots.
func (snaps *Snapshots) OldestLocal() *Snapshot {
	for snap := range snaps.Local() {
		return snap
	}
	return nil
}

// NewestLocal returns the newest local Snapshot.
// It returns nil if there are no local snapshots.
func (snaps *Snapshots) NewestLocal() *Snapshot {
	for snap := range snaps.LocalDesc() {
		return snap
	}
	return nil
}

// OldestRemote returns the oldest remote Snapshot.
// It returns nil if there are no remote snapshots.
func (snaps *Snapshots) OldestRemote() *Snapshot {
	for snap := range snaps.Remote() {
		return snap
	}
	return nil
}

// NewestRemote returns the newest remote Snapshot.
// It returns nil if there are no remote snapshots.
func (snaps *Snapshots) NewestRemote() *Snapshot {
	for snap := range snaps.RemoteDesc() {
		return snap
	}
	return nil
}
