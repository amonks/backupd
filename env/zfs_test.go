//go:build unix

package env

import (
	"slices"
	"strings"
	"testing"

	"monks.co/backupd/logger"
)

// TestGetDatasetsReadsChildrenShare: the dataset listing asks zfs for
// usedbychildren alongside used, so a caller can tell what a dataset
// holds on its own from what its descendants hold beneath it.
func TestGetDatasetsReadsChildrenShare(t *testing.T) {
	r := &recorder{rows: []string{
		"data/tank\t100\t10\t90",
		"data/tank/tm\t90\t20\t60",
		"data/tank/tm/brigid\t60\t60\t0",
	}}
	zfs := NewZFS("data", r)
	got, err := zfs.GetDatasets(logger.New("test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected one zfs call, got %v", r.calls)
	}
	want := []string{"zfs", "list", "-H", "-p", "-t", "filesystem", "-o", "name,used,logicalreferenced,usedbychildren", "-d", "1000", "data"}
	if !slices.Equal(r.calls[0], want) {
		t.Errorf("zfs call = %v, want %v", r.calls[0], want)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 datasets, got %d", len(got))
	}
	tm := got[1]
	if tm.Name != "/tank/tm" {
		t.Errorf("name = %q", tm.Name)
	}
	if tm.Size.Used != 90 || tm.Size.LogicalReferenced != 20 || tm.Size.UsedByChildren != 60 {
		t.Errorf("size = %+v", *tm.Size)
	}
	if tm.Size.Own() != 30 {
		t.Errorf("own = %d, want 30", tm.Size.Own())
	}
}

// A snapshot's name reaches zfs as one argument, whatever it holds. It
// is built from the periodicity a caller asks for, and splitting the
// command on whitespace once turned a periodicity holding a space into
// more operands for a root-run `zfs snapshot -r` — a recursive snapshot
// of any dataset in the pool, under a name no retention would prune.
func TestCreateSnapshotPassesTheNameWhole(t *testing.T) {
	r := &recorder{}
	zfs := NewZFS("data", r)
	if err := zfs.CreateSnapshot(logger.New("test"), "data/tank", "daily data/other@x"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected one zfs call, got %v", r.calls)
	}
	call := r.calls[0]
	if len(call) != 4 || !slices.Equal(call[:3], []string{"zfs", "snapshot", "-r"}) {
		t.Fatalf("zfs call = %q, want zfs snapshot -r and one name", call)
	}
	if !strings.HasPrefix(call[3], "data/tank@daily data/other@x-") {
		t.Errorf("name = %q, want the root, the periodicity, and a timestamp", call[3])
	}
}
