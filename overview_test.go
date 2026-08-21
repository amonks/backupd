package main

import (
	"testing"
	"time"

	"monks.co/backupd/history"
	"monks.co/backupd/logger"
)

// stripCycles reverses newest-first history into oldest-first render
// order and caps the strip.
func TestStripCycles(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var cycles []history.Cycle // newest first, like History.Cycles
	for i := 9; i >= 0; i-- {
		cycles = append(cycles, history.Cycle{StartedAt: base.Add(time.Duration(i) * time.Hour)})
	}

	out := stripCycles(cycles, 4)
	if len(out) != 4 {
		t.Fatalf("expected 4 cycles, got %d", len(out))
	}
	// The 4 newest, oldest of those first.
	for i, c := range out {
		want := base.Add(time.Duration(6+i) * time.Hour)
		if !c.StartedAt.Equal(want) {
			t.Errorf("out[%d].StartedAt = %s, want %s", i, c.StartedAt, want)
		}
	}

	if got := stripCycles(cycles[:2], 4); len(got) != 2 || !got[0].StartedAt.Equal(base.Add(8*time.Hour)) {
		t.Errorf("short input should reverse whole list, got %+v", got)
	}
}

func TestTailLogs(t *testing.T) {
	var logs []logger.LogEntry
	for i := range 5 {
		logs = append(logs, logger.LogEntry{Log: string(rune('a' + i))})
	}
	tail, elided := tailLogs(logs, 3)
	if elided != 2 || len(tail) != 3 || tail[0].Log != "c" {
		t.Errorf("tailLogs = %v (elided %d)", tail, elided)
	}
	tail, elided = tailLogs(logs, 10)
	if elided != 0 || len(tail) != 5 {
		t.Errorf("tailLogs under cap = %v (elided %d)", tail, elided)
	}
}
