package main

import (
	"reflect"
	"testing"
)

func TestSnapshots_NewSnapshots(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snaps := NewSnapshots(snapshot1, snapshot2)

	count := 0
	for snap := range snaps.All() {
		count++
		if snap.Name != "snap1" && snap.Name != "snap2" {
			t.Errorf("Unexpected snapshot name: %v", snap.Name)
		}
	}

	if count != 2 {
		t.Errorf("Expected 2 snapshots, got %d", count)
	}
}

func TestSnapshots_Add(t *testing.T) {
	snaps := NewSnapshots()

	snapshot1 := createSnapshot("dataset1", "snap1", 300)
	snapshot2 := createSnapshot("dataset2", "snap2", 100)
	snapshot3 := createSnapshot("dataset3", "snap3", 200)

	snaps.Add(snapshot1)
	snaps.Add(snapshot2)
	snaps.Add(snapshot3)

	snapNames := []string{"snap2", "snap3", "snap1"}
	idx := 0

	for snap := range snaps.All() {
		if snap.Name != snapNames[idx] {
			t.Errorf("Expected snapshot name: %s, got: %s", snapNames[idx], snap.Name)
		}
		idx++
	}
}

func TestSnapshots_AddNewHead(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 200)
	snapshot2 := createSnapshot("dataset1", "snap2", 300)
	snapshotNewHead := createSnapshot("dataset1", "snap0", 100) // This should become the new head

	snaps := NewSnapshots(snapshot1, snapshot2)
	snaps.Add(snapshotNewHead)

	snapNames := []string{"snap0", "snap1", "snap2"}
	idx := 0

	for snap := range snaps.All() {
		if snap.Name != snapNames[idx] {
			t.Errorf("Expected snapshot name: %s, got: %s", snapNames[idx], snap.Name)
		}
		idx++
	}
}

func TestSnapshots_AddNewTail(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snapshotNewTail := createSnapshot("dataset1", "snap3", 300) // This should become the new tail

	snaps := NewSnapshots(snapshot1, snapshot2)
	snaps.Add(snapshotNewTail)

	snapNames := []string{"snap1", "snap2", "snap3"}
	idx := 0

	for snap := range snaps.All() {
		if snap.Name != snapNames[idx] {
			t.Errorf("Expected snapshot name: %s, got: %s", snapNames[idx], snap.Name)
		}
		idx++
	}
}

func TestSnapshots_AddInMiddle(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot3 := createSnapshot("dataset1", "snap3", 300)
	snapshotInMiddle := createSnapshot("dataset1", "snap2", 200) // This should be in the middle

	snaps := NewSnapshots(snapshot1, snapshot3)
	snaps.Add(snapshotInMiddle)

	snapNames := []string{"snap1", "snap2", "snap3"}
	idx := 0

	for snap := range snaps.All() {
		if snap.Name != snapNames[idx] {
			t.Errorf("Expected snapshot name: %s, got: %s", snapNames[idx], snap.Name)
		}
		idx++
	}
}

func TestSnapshots_AddDuplicate(t *testing.T) {
	snaps := NewSnapshots()
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snaps.Add(snapshot1)
	snaps.Add(snapshot1)

	count := 0
	for range snaps.All() {
		count++
	}

	if count != 1 {
		t.Errorf("Expected 1 snapshot, since duplicate was added. Got %d", count)
	}
}

func TestSnapshots_Del(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snaps := NewSnapshots(snapshot1, snapshot2)

	snaps.Del("snap1")
	count := 0
	expectedRemaining := "snap2"

	for snap := range snaps.All() {
		count++
		if snap.Name != expectedRemaining {
			t.Errorf("Expected remaining snapshot name: %s, got: %s", expectedRemaining, snap.Name)
		}
	}

	if count != 1 {
		t.Errorf("Expected 1 remaining snapshot, got %d", count)
	}
}

func TestSnapshots_DelNonExistent(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snaps := NewSnapshots(snapshot1, snapshot2)

	snaps.Del("nonexistent")
	count := 0

	for snap := range snaps.All() {
		count++
		if snap.Name != "snap1" && snap.Name != "snap2" {
			t.Errorf("Unexpected snapshot name: %v", snap.Name)
		}
	}

	if count != 2 {
		t.Errorf("Expected 2 snapshots, got %d", count)
	}
}

func TestSnapshots_OrderAfterDeletion(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snapshot3 := createSnapshot("dataset1", "snap3", 300)
	snaps := NewSnapshots(snapshot1, snapshot2, snapshot3)

	snaps.Del("snap2")

	snapNames := []string{"snap1", "snap3"}
	idx := 0

	for snap := range snaps.All() {
		if snap.Name != snapNames[idx] {
			t.Errorf("Expected snapshot name: %s, got: %s", snapNames[idx], snap.Name)
		}
		idx++
	}
}

func TestSnapshots_All(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.All() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot1, snapshot2, snapshot3}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected all snapshots to match. Got %#v", results)
	}
}

func TestSnapshots_Local(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.Local() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot1, snapshot3}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected local snapshots to match. Got %#v", results)
	}
}

func TestSnapshots_Remote(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.Remote() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot2, snapshot3}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected remote snapshots to match. Got %#v", results)
	}
}

func TestSnapshots_AllDesc(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.AllDesc() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot3, snapshot2, snapshot1}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected all snapshots (descending) to match. Got %#v", results)
	}
}

func TestSnapshots_LocalDesc(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.LocalDesc() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot3, snapshot1}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected local snapshots (descending) to match. Got %#v", results)
	}
}

func TestSnapshots_RemoteDesc(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	var results []*Snapshot
	for snap := range snapshots.RemoteDesc() {
		results = append(results, snap)
	}

	expected := []*Snapshot{snapshot3, snapshot2}
	if !reflect.DeepEqual(results, expected) {
		t.Errorf("Expected remote snapshots (descending) to match. Got %#v", results)
	}
}

func TestSnapshots_Len(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset2", "snap2", 200)
	snapshot3 := createSnapshot("dataset3", "snap3", 300)
	snaps := NewSnapshots(snapshot1, snapshot2, snapshot3)

	expectedLen := 3
	gotLen := snaps.Len()

	if gotLen != expectedLen {
		t.Errorf("Expected length: %d, got: %d", expectedLen, gotLen)
	}
}

func TestSnapshots_Has(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset2", "snap2", 200)
	snaps := NewSnapshots(snapshot1, snapshot2)

	if !snaps.Has(snapshot1) {
		t.Errorf("Expected snapshot %s to be present", snapshot1.Name)
	}

	if !snaps.Has(snapshot2) {
		t.Errorf("Expected snapshot %s to be present", snapshot2.Name)
	}

	snapshot3 := createSnapshot("dataset3", "snap3", 300)
	if snaps.Has(snapshot3) {
		t.Errorf("Expected snapshot %s to not be present", snapshot3.Name)
	}
}

func createSnapshot(dataset, name string, createdAt int64) *Snapshot {
	return &Snapshot{
		Dataset:   dataset,
		Name:      name,
		CreatedAt: createdAt,
	}
}

func TestSnapshots_LenLocal(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	expectedLenLocal := 2
	gotLenLocal := snapshots.LenLocal()

	if gotLenLocal != expectedLenLocal {
		t.Errorf("Expected LenLocal: %d, got: %d", expectedLenLocal, gotLenLocal)
	}
}

func TestSnapshots_LenRemote(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: True(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	expectedLenRemote := 2
	gotLenRemote := snapshots.LenRemote()

	if gotLenRemote != expectedLenRemote {
		t.Errorf("Expected LenRemote: %d, got: %d", expectedLenRemote, gotLenRemote)
	}
}

func TestSnapshots_Oldest(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snapshot3 := createSnapshot("dataset1", "snap3", 300)
	snaps := NewSnapshots(snapshot1, snapshot2, snapshot3)

	oldest := snaps.Oldest()
	if oldest == nil || oldest.Name != "snap1" {
		t.Errorf("Expected oldest snapshot to be 'snap1', got '%v'", oldest)
	}

	snaps = NewSnapshots()
	oldest = snaps.Oldest()
	if oldest != nil {
		t.Errorf("Expected nil for oldest snapshot in empty set, got '%v'", oldest)
	}
}

func TestSnapshots_Newest(t *testing.T) {
	snapshot1 := createSnapshot("dataset1", "snap1", 100)
	snapshot2 := createSnapshot("dataset1", "snap2", 200)
	snapshot3 := createSnapshot("dataset1", "snap3", 300)
	snaps := NewSnapshots(snapshot1, snapshot2, snapshot3)

	newest := snaps.Newest()
	if newest == nil || newest.Name != "snap3" {
		t.Errorf("Expected newest snapshot to be 'snap3', got '%v'", newest)
	}

	snaps = NewSnapshots()
	newest = snaps.Newest()
	if newest != nil {
		t.Errorf("Expected nil for newest snapshot in empty set, got '%v'", newest)
	}
}

func TestSnapshots_OldestLocal(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625100000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	oldestLocal := snapshots.OldestLocal()
	if oldestLocal == nil || oldestLocal.Name != "snap2" {
		t.Errorf("Expected oldest local snapshot to be 'snap2', got '%v'", oldestLocal)
	}
}

func TestSnapshots_NewestLocal(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: False()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: True(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: False(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	newestLocal := snapshots.NewestLocal()
	if newestLocal == nil || newestLocal.Name != "snap2" {
		t.Errorf("Expected newest local snapshot to be 'snap2', got '%v'", newestLocal)
	}
}

func TestSnapshots_OldestRemote(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: True()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625100000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: False(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	oldestRemote := snapshots.OldestRemote()
	if oldestRemote == nil || oldestRemote.Name != "snap2" {
		t.Errorf("Expected oldest remote snapshot to be 'snap2', got '%v'", oldestRemote)
	}
}

func TestSnapshots_NewestRemote(t *testing.T) {
	snapshot1 := &Snapshot{Dataset: "ds1", Name: "snap1", CreatedAt: 1625200000, IsOnLocal: True(), IsOnRemote: True()}
	snapshot2 := &Snapshot{Dataset: "ds2", Name: "snap2", CreatedAt: 1625300000, IsOnLocal: False(), IsOnRemote: True()}
	snapshot3 := &Snapshot{Dataset: "ds3", Name: "snap3", CreatedAt: 1625400000, IsOnLocal: False(), IsOnRemote: True()}
	snapshots := NewSnapshots(snapshot1, snapshot2, snapshot3)

	newestRemote := snapshots.NewestRemote()
	if newestRemote == nil || newestRemote.Name != "snap3" {
		t.Errorf("Expected newest remote snapshot to be 'snap3', got '%v'", newestRemote)
	}
}
