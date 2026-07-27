package sim

import (
	"fmt"
	"time"

	"monks.co/backupd/model"
)

// Demo builds the scenario behind `backupd -sim`: a small fleet of
// datasets in every interesting state — in sync, backlogged, awaiting
// an initial transfer, paused with pending work, failing with a stale
// backup — plus one transfer that will be interrupted mid-stream to
// demonstrate resume handling. Transfers are paced so progress bars
// are visible.
func Demo(now time.Time) *Sim {
	s := New()
	s.Rate = 48 << 20 // ~48 MB/s: a few hundred MB transfers in seconds

	snap := func(ds model.DatasetName, period string, t time.Time, size int64) *model.Snapshot {
		return &model.Snapshot{
			Dataset:           ds,
			Name:              fmt.Sprintf("%s-%s", period, t.Format("2006-01-02-15:04:05")),
			CreatedAt:         t.Unix(),
			LogicalReferenced: size,
		}
	}
	dailyAt := func(daysAgo int) time.Time {
		day := now.AddDate(0, 0, -daysAgo)
		return time.Date(day.Year(), day.Month(), day.Day(), 1, 0, 0, 0, day.Location())
	}

	// dataset seeds `days` daily snapshots; the remote has all but the
	// newest `behind` of them (a backlog of `behind` pending transfers).
	dataset := func(name model.DatasetName, size int64, days, behind int) {
		var locals, remotes []*model.Snapshot
		for i := days; i >= 1; i-- {
			sn := snap(name, "daily", dailyAt(i), size)
			locals = append(locals, sn)
			if i > behind {
				remotes = append(remotes, sn)
			}
		}
		s.SeedLocal(name, locals...)
		if len(remotes) > 0 {
			s.SeedRemote(name, remotes...)
		}
	}

	dataset("", 1<<30, 10, 0)              // root, in sync
	dataset("/media", 600<<20, 10, 3)      // backlogged: 3 transfers pending
	dataset("/media/photos", 300<<20, 10, 0)
	dataset("/home", 200<<20, 10, 0)
	dataset("/tm", 1<<20, 3, 0)            // paused subtree (see demo config)
	dataset("/tm/lugh", 2<<30, 5, 2)       // paused with pending work it won't do

	// /db's receives fail persistently (a corrupted stream), so its
	// backlog never drains: the dashboard shows a failing dataset whose
	// backup keeps aging — the failing and stale-backup issues at once.
	dataset("/db", 80<<20, 10, 6)
	s.SetDatasetError("/db", "cannot receive incremental stream: invalid backup stream (checksum mismatch)")

	// /home/thor exists only locally: its whole history awaits an
	// initial transfer.
	s.SeedLocal("/home/thor",
		snap("/home/thor", "daily", dailyAt(2), 500<<20),
		snap("/home/thor", "daily", dailyAt(1), 500<<20),
	)

	// The first transfer attempt is cut off at 60%: one failed cycle,
	// then the next cycle resumes and completes it.
	s.InterruptNextTransfer(0.6)

	return s
}
