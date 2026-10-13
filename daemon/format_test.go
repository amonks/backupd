package daemon

import (
	"strings"
	"testing"
	"time"

	"monks.co/backupd/status"
	"monks.co/backupd/view"
)

// countdownCases is the countdown's wording, which fmtCountdown renders
// and the page's formatUntil keeps ticking: browser_test.go pins the
// script to the same table, so the first tick never changes the words.
var countdownCases = []struct {
	d    time.Duration
	want string
}{
	{0, "any moment"},
	{999 * time.Millisecond, "any moment"},
	{-5 * time.Second, "any moment"},
	{5 * time.Second, "in 5s"},
	{5900 * time.Millisecond, "in 5s"},
	{4 * time.Minute, "in 4m"},
	{4*time.Minute + 23*time.Second, "in 4m23s"},
	{time.Hour, "in 1h"},
	{time.Hour + 5*time.Minute + 9*time.Second, "in 1h5m"},
	{48 * time.Hour, "in 2d"},
	{51*time.Hour + 30*time.Minute, "in 2d3h"},
}

func TestFmtCountdown(t *testing.T) {
	for _, c := range countdownCases {
		if got := fmtCountdown(c.d); got != c.want {
			t.Errorf("fmtCountdown(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

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
	if got := activityLabel(a, view.CycleProgress{}); got != "syncing /foo — step 2 of 5: transfer x" {
		t.Errorf("unexpected syncing label %q", got)
	}
	// With a cycle queue, the label places the dataset in the cycle.
	c := view.CycleProgress{Total: 12, Position: 4, HasActive: true, Active: "/foo"}
	if got := activityLabel(a, c); got != "syncing /foo (4 of 12) — step 2 of 5: transfer x" {
		t.Errorf("unexpected positioned label %q", got)
	}
	a = status.Activity{Phase: status.BackingOff, ConsecutiveFailures: 3}
	if got := activityLabel(a, view.CycleProgress{}); got != "3 cycles failed — retrying" {
		t.Errorf("unexpected backoff label %q", got)
	}
}
