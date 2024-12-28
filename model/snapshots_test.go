package model

import (
	"testing"
	"time"
)

func TestSnapshots_Add(t *testing.T) {
	snaps := NewSnapshots()

	// add to empty
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snaps.Add(snap1)
	if snaps.Len() != 1 || !snaps.Has(snap1) {
		t.Errorf("Expected snapshot to be added. Got: %d", snaps.Len())
	}

	// add before head
	snap0 := &Snapshot{Name: "snap0", CreatedAt: 0}
	snaps.Add(snap0)
	if snaps.Oldest() != snap0 {
		t.Errorf("Expected snap0 to be the oldest snapshot")
	}

	// add after tail
	snap3 := &Snapshot{Name: "snap3", CreatedAt: 3}
	snaps.Add(snap3)
	if snaps.Newest() != snap3 {
		t.Errorf("Expected snap3 to be the newest snapshot")
	}

	// add in the middle
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}
	snaps.Add(snap2)

	// validate
	var got []*Snapshot
	for snap := range snaps.All() {
		got = append(got, snap)
	}
	expected := []*Snapshot{snap0, snap1, snap2, snap3}
	for i := range expected {
		if got[i] != expected[i] {
			t.Errorf("Expected %v, but got %v at index %d", expected[i], got[i], i)
		}
	}
}

func TestSnapshots_Del(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}
	snap3 := &Snapshot{Name: "snap3", CreatedAt: 3}

	snaps := NewSnapshots(snap1, snap2, snap3)

	// delete head
	snaps.Del(snap1)
	if snaps.Len() != 2 || snaps.Oldest() != snap2 {
		t.Errorf("Expected snap2 to be the oldest after deleting snap1")
	}

	// delete tail
	snaps.Del(snap3)
	if snaps.Len() != 1 || snaps.Newest() != snap2 {
		t.Errorf("Expected snap2 to be the newest after deleting snap3")
	}

	// delete only element
	snaps.Del(snap2)
	if snaps.Len() != 0 {
		t.Errorf("Expected no snapshots after deleting all")
	}

	// delete non-existing
	snaps.Del(&Snapshot{Name: "something"})
	if snaps.Len() != 0 {
		t.Errorf("Expected length to remain zero after attempting to delete non-existent snapshot")
	}
}

func TestSnapshots_OldestNewest(t *testing.T) {
	currentTime := time.Now().Unix()
	snap1 := &Snapshot{Name: "snap1", CreatedAt: currentTime - 100}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: currentTime - 50}
	snap3 := &Snapshot{Name: "snap3", CreatedAt: currentTime}

	snaps := NewSnapshots(snap1, snap2, snap3)

	if snaps.Oldest() != snap1 {
		t.Errorf("Expected snap1 as the oldest but got %v", snaps.Oldest())
	}
	if snaps.Newest() != snap3 {
		t.Errorf("Expected snap3 as the newest but got %v", snaps.Newest())
	}
}

func TestSnapshots_Has(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}

	snaps := NewSnapshots(snap1)

	if !snaps.Has(snap1) {
		t.Errorf("Expected to have snap1")
	}

	if snaps.Has(snap2) {
		t.Errorf("Did not expect to have snap2")
	}
}

func TestSnapshots_Union(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}
	snap3 := &Snapshot{Name: "snap3", CreatedAt: 3}

	snaps1 := NewSnapshots(snap1, snap2)
	snaps2 := NewSnapshots(snap3)

	union := snaps1.Union(snaps2)

	if union.Len() != 3 || !union.Has(snap1) || !union.Has(snap2) || !union.Has(snap3) {
		t.Errorf("Expected union to contain all snapshots")
	}
}

func TestSnapshots_Intersection(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}

	snaps1 := NewSnapshots(snap1, snap2)
	snaps2 := NewSnapshots(snap2)

	intersection := snaps1.Intersection(snaps2)

	if intersection.Len() != 1 || !intersection.Has(snap2) {
		t.Errorf("Expected intersection to contain only snap2")
	}
}

func TestSnapshots_Difference(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}

	snaps1 := NewSnapshots(snap1, snap2)
	snaps2 := NewSnapshots(snap2)

	difference := snaps1.Difference(snaps2)

	if difference.Len() != 1 || !difference.Has(snap1) {
		t.Errorf("Expected difference to contain only snap1")
	}
}
func TestSnapshots_GroupByAdjacency(t *testing.T) {
	snap1 := &Snapshot{Name: "snap1", CreatedAt: 1}
	snap2 := &Snapshot{Name: "snap2", CreatedAt: 2}
	snap3 := &Snapshot{Name: "snap3", CreatedAt: 3}
	snap4 := &Snapshot{Name: "snap4", CreatedAt: 4}

	snaps1 := NewSnapshots(snap1, snap2, snap3, snap4)
	subset := NewSnapshots(snap1, snap2, snap4)

	groups := snaps1.GroupByAdjacency(subset)

	if len(groups) != 2 {
		t.Errorf("Expected 2 groups, but got %d", len(groups))
	}

	if groups[0].Len() != 2 {
		t.Errorf("Expected first group to have length 2, but got %d", groups[0].Len())
	}
	if groups[0].Oldest() != snap1 {
		t.Errorf("Expected first group's oldest to be snap1, but got %v", groups[0].Oldest())
	}
	if groups[0].Newest() != snap2 {
		t.Errorf("Expected first group's newest to be snap2, but got %v", groups[0].Newest())
	}

	if groups[1].Len() != 1 {
		t.Errorf("Expected second group to have length 1, but got %d", groups[1].Len())
	}
	if groups[1].Oldest() != snap4 {
		t.Errorf("Expected second group's oldest to be snap4, but got %v", groups[1].Oldest())
	}
}
