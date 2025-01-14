package progress

import (
	"testing"

	"monks.co/backupd/model"
)

func TestProgress(t *testing.T) {
	progress := New()
	progress.Log(model.DatasetName("a"), "hello %s", "world")
	progress.Log(model.DatasetName("a"), "hello %s", "stars")
	progress.Log(model.DatasetName("b"), "goodbye %s", "moon")
	progress.Log(model.DatasetName("b"), "goodbye %s", "trees")

	logs := progress.Deref()
	if len(logs) != 2 {
		t.Fatalf("len(logs) = %d; want 2", len(logs))
	}

	if len(logs["a"]) != 2 {
		t.Errorf(`len(logs["a"]) = %d; want 2`, len(logs["a"]))
	}
	if logs["a"][0].Log != "hello world" {
		t.Errorf(`logs["a"][0].Log = "%s"; want "hello world"`, logs["a"][0])
	}
	if logs["a"][1].Log != "hello stars" {
		t.Errorf(`logs["a"][1].Log = "%s"; want "hello stars"`, logs["a"][1])
	}

	if len(logs["b"]) != 2 {
		t.Errorf(`len(logs["b"]) = %d; want 2`, len(logs["b"]))
	}
	if logs["b"][0].Log != "goodbye moon" {
		t.Errorf(`logs["b"][0].Log = "%s"; want "goodbye moon"`, logs["b"][0])
	}
	if logs["b"][1].Log != "goodbye trees" {
		t.Errorf(`logs["b"][1].Log = "%s"; want "goodbye trees"`, logs["b"][1])
	}
}
