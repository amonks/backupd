package sim

import (
	"testing"

	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

// TestListDatasetsNestsLikeZFS: the sim reports sizes the way zfs does
// — a dataset's used carries every descendant, and usedbychildren
// says how much of it is theirs — so the daemon's totals are exercised
// against the same shape of numbers they meet on a real pool.
func TestListDatasetsNestsLikeZFS(t *testing.T) {
	s := New()
	s.LocalCapacity = 1000
	snap := func(ds model.DatasetName, size int64) *model.Snapshot {
		return &model.Snapshot{Dataset: ds, Name: "daily-t01", CreatedAt: 1, LogicalReferenced: size}
	}
	s.SeedLocal("", snap("", 1)) // the tracked tree's root, as it ships
	s.SeedLocal("/tank", snap("/tank", 10))
	s.SeedLocal("/tank/tm", snap("/tank/tm", 20))
	s.SeedLocal("/tank/tm/brigid", snap("/tank/tm/brigid", 60))
	s.SeedLocal("/tankard", snap("/tankard", 5)) // a sibling that shares a name prefix, not a child

	got, err := s.LocalDatasets(logger.New("test"))
	if err != nil {
		t.Fatal(err)
	}
	// Every dataset has the pool's room: the capacity less the 96
	// bytes the pool holds.
	want := map[model.DatasetName]model.DatasetSize{
		"":                {Used: 96, LogicalReferenced: 1, UsedByChildren: 95, Available: 904},
		"/tank":           {Used: 90, LogicalReferenced: 10, UsedByChildren: 80, Available: 904},
		"/tank/tm":        {Used: 80, LogicalReferenced: 20, UsedByChildren: 60, Available: 904},
		"/tank/tm/brigid": {Used: 60, LogicalReferenced: 60, Available: 904},
		"/tankard":        {Used: 5, LogicalReferenced: 5, Available: 904},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d datasets, want %d", len(got), len(want))
	}
	for _, info := range got {
		if *info.Size != want[info.Name] {
			t.Errorf("%s: size %+v, want %+v", info.Name, *info.Size, want[info.Name])
		}
	}
}

// TestListDatasetsReportsRoomLeft: the remote's listing reports its
// pool's room on every dataset, and none once the remote is full —
// what a dashboard watching for a filling remote reads.
func TestListDatasetsReportsRoomLeft(t *testing.T) {
	s := New()
	s.RemoteCapacity = 1000
	s.SeedRemote("", &model.Snapshot{Dataset: "", Name: "daily-t01", CreatedAt: 1, LogicalReferenced: 300})
	s.SeedRemote("/tank", &model.Snapshot{Dataset: "/tank", Name: "daily-t01", CreatedAt: 1, LogicalReferenced: 200})

	available := func() map[model.DatasetName]int64 {
		t.Helper()
		got, err := s.RemoteDatasets(logger.New("test"))
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.DatasetName]int64{}
		for _, info := range got {
			out[info.Name] = info.Size.Available
		}
		return out
	}
	if got := available(); got[""] != 500 || got["/tank"] != 500 {
		t.Errorf("available = %v, want 500 on every dataset", got)
	}
	s.SetRemoteFull(true)
	if got := available(); got[""] != 0 || got["/tank"] != 0 {
		t.Errorf("a full remote's available = %v, want 0", got)
	}
}
