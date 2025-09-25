package progress

import (
	"testing"
)

func TestProcessLogs(t *testing.T) {
	logs := NewProcessLogs()

	logs.Log("start")
	logs.Log("hello %s", "world")
	logs.Log("hello %s", "stars")
	logs.Log("end")

	entries := logs.GetLogs()
	if len(entries) != 4 {
		t.Fatalf("len(entries) = %d; want 4", len(entries))
	}

	// Check that logs are in order
	if entries[0].Log != "start" {
		t.Errorf("first log = %s; want 'start'", entries[0].Log)
	}
	if entries[1].Log != "hello world" {
		t.Errorf("second log = %s; want 'hello world'", entries[1].Log)
	}
	if entries[2].Log != "hello stars" {
		t.Errorf("third log = %s; want 'hello stars'", entries[2].Log)
	}
	if entries[3].Log != "end" {
		t.Errorf("fourth log = %s; want 'end'", entries[3].Log)
	}
}

func TestProcessLogger(t *testing.T) {
	logs := NewProcessLogs()
	logger := logs.Logger("test")

	logger.Printf("test message %d", 123)
	logger.Printf("another message")

	entries := logs.GetLogs()
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d; want 2", len(entries))
	}

	if entries[0].Log != "test message 123" {
		t.Errorf("first log = %s; want 'test message 123'", entries[0].Log)
	}
	if entries[1].Log != "another message" {
		t.Errorf("second log = %s; want 'another message'", entries[1].Log)
	}
}