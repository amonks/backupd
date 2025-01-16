package progress

import (
	"testing"

	"monks.co/backupd/model"
)

func TestProgress(t *testing.T) {
	progress := New()
	progress.Log(model.DatasetName("global"), "start")
	progress.Log(model.DatasetName("a"), "hello %s", "world")
	progress.Log(model.DatasetName("a"), "hello %s", "stars")
	progress.Done(model.DatasetName("a"))
	progress.Log(model.DatasetName("global"), "end")

	logs := progress.Deref()
	if len(logs) != 2 {
		t.Fatalf("len(logs) = %d; want 2", len(logs))
	}

	aLogs := logs.Get(model.DatasetName("a"))
	if len(aLogs) != 2 {
		t.Fatalf("len(aLogs) = %d; want 2", len(aLogs))
	}

	n := 0
	for _, logs := range logs.All() {
		n++
		if len(logs) != 2 {
			t.Fatalf("len(logs) = %d; want 2", len(logs))
		}
	}
	if n != 2 {
		t.Fatalf("n = %d; want 2", n)
	}
}
