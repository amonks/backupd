package main

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"

	"monks.co/backupd/status"
	"monks.co/backupd/view"
)

// Formatting helpers for the dashboard templates.

// fmtBytes renders a byte count compactly ("1.2 GB"), or "-" for zero.
func fmtBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	return humanize.Bytes(uint64(n))
}

// fmtAgo renders a past time relative to now ("3 hours ago"), or "never"
// for the zero time.
func fmtAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return humanize.Time(t)
}

// fmtCompactDuration renders a duration in its two most significant
// units: "<1m", "42m", "3h12m", "2d4h".
func fmtCompactDuration(d time.Duration) string {
	return view.CompactDuration(d)
}

// plural renders "1 cycle" / "3 cycles".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// fmtUntil renders a future time as a countdown ("in 42m"); past
// deadlines render as "any moment".
func fmtUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "any moment"
	}
	return "in " + fmtCompactDuration(d)
}

// fmtRate renders bytes/sec as bits/sec, matching how transfer speeds
// are usually discussed ("98 Mbps").
func fmtRate(bytesPerSec float64) string {
	return humanize.SIWithDigits(bytesPerSec*8, 1, "bps")
}

// transferLabel summarizes an in-flight transfer:
// "42% · 1.2 GB of 3.0 GB · 98 Mbps · ETA 12m".
func transferLabel(x status.Transfer) string {
	out := fmt.Sprintf("%.0f%% · %s of %s",
		x.Percent(),
		humanize.Bytes(uint64(max(x.Sent, 0))),
		humanize.Bytes(uint64(max(x.Total, 0))))
	if x.Rate > 0 {
		out += " · " + fmtRate(x.Rate)
	}
	if eta, ok := x.ETA(); ok {
		out += " · ETA " + fmtCompactDuration(eta)
	}
	return out
}

// activityLabel is the status strip's one-line description of what the
// daemon is doing right now, including where in the cycle it is.
func activityLabel(a status.Activity, c view.CycleProgress) string {
	switch a.Phase {
	case status.Starting:
		return "starting up"
	case status.Refreshing:
		return "refreshing datasets from ZFS"
	case status.Syncing:
		label := fmt.Sprintf("syncing %s", a.Dataset)
		if c.Total > 1 && c.HasActive {
			label += fmt.Sprintf(" (%d of %d)", c.Position, c.Total)
		}
		if a.Steps > 0 {
			label += fmt.Sprintf(" — step %d of %d: %s", a.Step, a.Steps, a.Operation)
		}
		return label
	case status.Idle:
		return "idle — next cycle"
	case status.BackingOff:
		return fmt.Sprintf("%s failed — retrying", plural(a.ConsecutiveFailures, "cycle"))
	default:
		return ""
	}
}
