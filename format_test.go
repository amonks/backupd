package main

import (
	"strings"
	"testing"
	"time"

	"monks.co/backupd/status"
)

func TestFmtCompactDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{42 * time.Minute, "42m"},
		{3*time.Hour + 12*time.Minute, "3h12m"},
		{4 * time.Hour, "4h"},
		{52 * time.Hour, "2d4h"},
		{48 * time.Hour, "2d"},
		{-42 * time.Minute, "42m"},
	}
	for _, c := range cases {
		if got := fmtCompactDuration(c.d); got != c.want {
			t.Errorf("fmtCompactDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestTransferLabel(t *testing.T) {
	x := status.Transfer{Sent: 512, Total: 1024, Rate: 256}
	label := transferLabel(x)
	for _, want := range []string{"50%", "512 B of 1.0 kB", "bps", "ETA"} {
		if !strings.Contains(label, want) {
			t.Errorf("expected %q in %q", want, label)
		}
	}
}

func TestActivityLabel(t *testing.T) {
	a := status.Activity{Phase: status.Syncing, Dataset: "/foo", Step: 2, Steps: 5, Operation: "transfer x"}
	if got := activityLabel(a); got != "syncing /foo — step 2 of 5: transfer x" {
		t.Errorf("unexpected syncing label %q", got)
	}
	a = status.Activity{Phase: status.BackingOff, ConsecutiveFailures: 3}
	if got := activityLabel(a); got != "3 cycles failed — retrying" {
		t.Errorf("unexpected backoff label %q", got)
	}
}
