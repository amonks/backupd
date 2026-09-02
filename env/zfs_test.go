//go:build unix

package env

import (
	"slices"
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
