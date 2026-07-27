package view

import (
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

// The rapid tests treat the derivation layer as a specification: for
// arbitrary inventories, policies, histories, and activities, every
// claim the dashboard makes must follow from the state it was derived
// from — ages come from the inventory, issues appear exactly when
// their condition holds, and the system verdict is a pure function of
// the issue list.

var rapidTypes = []string{"hourly", "daily", "weekly", "monthly"}

// genSnaps generates a set of snapshots for one dataset with ages up
// to ~80 hours before now.
func genSnaps(t *rapid.T, label string, dataset model.DatasetName) []*model.Snapshot {
	n := rapid.IntRange(0, 12).Draw(t, label+"-n")
	byID := map[string]*model.Snapshot{}
	for range n {
		typ := rapid.SampledFrom(rapidTypes).Draw(t, label+"-type")
		hour := rapid.IntRange(0, 80).Draw(t, label+"-hour")
		snap := &model.Snapshot{
			Dataset:   dataset,
			Name:      fmt.Sprintf("%s-t%02d", typ, hour),
			CreatedAt: now.Add(-time.Duration(hour) * time.Hour).Unix(),
		}
		byID[snap.ID()] = snap
	}
	var out []*model.Snapshot
	for _, snap := range byID {
		out = append(out, snap)
	}
	return out
}

func genRapidPolicy(t *rapid.T, label string) map[string]int {
	policy := map[string]int{}
	for _, typ := range rapidTypes {
		if rapid.Bool().Draw(t, label+"-has-"+typ) {
			policy[typ] = rapid.IntRange(0, 5).Draw(t, label+"-n-"+typ)
		}
	}
	return policy
}

func genConf(t *rapid.T) *config.Config {
	conf := &config.Config{}
	conf.Local.Policy = genRapidPolicy(t, "local")
	conf.Remote.Policy = genRapidPolicy(t, "remote")
	conf.Paused = rapid.Bool().Draw(t, "paused")
	if rapid.Bool().Draw(t, "snitch") {
		conf.SnitchID = "snitch"
	}
	return conf
}

func genInput(t *rapid.T) Input {
	names := []model.DatasetName{"/a", "/b", "/c"}
	count := rapid.IntRange(1, 3).Draw(t, "datasets")
	state := model.New()
	hist := history.New()
	for _, name := range names[:count] {
		local := genSnaps(t, "local"+string(name), name)
		state = model.AddLocalDataset(name, local, nil)(state)
		remote := genSnaps(t, "remote"+string(name), name)
		state = model.AddRemoteDataset(name, remote, nil)(state)

		// Arbitrary run history: maybe a success, maybe a failure, in
		// either order.
		if rapid.Bool().Draw(t, "success"+string(name)) {
			at := now.Add(-time.Duration(rapid.IntRange(0, 300).Draw(t, "sat"+string(name))) * time.Minute)
			hist.RecordDatasetSuccess(name, at)
		}
		if rapid.Bool().Draw(t, "failure"+string(name)) {
			at := now.Add(-time.Duration(rapid.IntRange(0, 300).Draw(t, "fat"+string(name))) * time.Minute)
			hist.RecordDatasetFailure(name, at, "induced failure")
		}
	}

	phase := rapid.SampledFrom([]status.Phase{
		status.Starting, status.Refreshing, status.Syncing, status.Idle, status.BackingOff,
	}).Draw(t, "phase")
	activity := status.Activity{Phase: phase}
	if phase == status.Syncing {
		activity.Dataset = rapid.SampledFrom(names[:count]).Draw(t, "syncing")
	}
	if phase == status.BackingOff {
		activity.ConsecutiveFailures = rapid.IntRange(1, 5).Draw(t, "consecutive")
	}

	if rapid.Bool().Draw(t, "cycle") {
		hist.RecordCycle(history.Cycle{
			StartedAt: now.Add(-time.Hour),
			StoppedAt: now.Add(-30 * time.Minute),
			OK:        rapid.Bool().Draw(t, "cycleOK"),
			Error:     "cycle error",
		})
	}
	if rapid.Bool().Draw(t, "snitched") {
		hist.RecordSnitch(now.Add(-time.Duration(rapid.IntRange(0, 600).Draw(t, "snitchAge")) * time.Minute))
	}

	return Input{
		State:    state,
		Conf:     genConf(t),
		History:  hist,
		Activity: activity,
		Now:      now,
		Boot:     now.Add(-time.Duration(rapid.IntRange(0, 600).Draw(t, "bootAge")) * time.Minute),
	}
}

// TestRapidAssuranceFromInventory: every age the dashboard shows is
// exactly the inventory's answer — restart-proof ground truth.
func TestRapidAssuranceFromInventory(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genInput(t)
		sys := Compute(in)

		if len(sys.Datasets) != len(in.State.ListDatasets()) {
			t.Fatalf("expected every dataset to appear exactly once")
		}
		for _, ds := range sys.Datasets {
			m := in.State.GetDataset(ds.Name)
			local, remote := m.Current.Local, m.Current.Remote

			if ds.HasLocal != (local.Len() > 0) || ds.HasRemote != (remote.Len() > 0) {
				t.Fatalf("%s: presence flags disagree with inventory", ds.Name)
			}
			if ds.HasLocal && ds.Snapshotted != in.Now.Sub(local.Newest().Time()) {
				t.Fatalf("%s: snapshotted %s != newest local age", ds.Name, ds.Snapshotted)
			}
			if ds.HasRemote {
				if ds.BackedUp != in.Now.Sub(remote.Newest().Time()) {
					t.Fatalf("%s: backed up %s != newest remote age", ds.Name, ds.BackedUp)
				}
				if ds.Depth != in.Now.Sub(remote.Oldest().Time()) {
					t.Fatalf("%s: depth %s != oldest remote age", ds.Name, ds.Depth)
				}
			}
			if ds.HasLocal && ds.HasRemote {
				if ds.Lag != ds.BackedUp-ds.Snapshotted {
					t.Fatalf("%s: lag %s != backedUp-snapshotted", ds.Name, ds.Lag)
				}
			}
			if ds.Unreplicated != (ds.HasLocal && !ds.HasRemote) {
				t.Fatalf("%s: unreplicated flag wrong", ds.Name)
			}

			// Staleness follows the cadence rule exactly: snapshots
			// against the local policy's cadence, the backup against
			// the remote policy's.
			localPolicy, remotePolicy, _ := in.Conf.PolicyFor(ds.Name.Path())
			cadence, backupCadence := Cadence(localPolicy), Cadence(remotePolicy)
			if ds.Cadence != cadence || ds.BackupCadence != backupCadence {
				t.Fatalf("%s: cadences %s/%s != policy cadences %s/%s",
					ds.Name, ds.Cadence, ds.BackupCadence, cadence, backupCadence)
			}
			wantSnapStale := cadence > 0 && (!ds.HasLocal || ds.Snapshotted > StaleFactor*cadence)
			if ds.SnapshotsStale != wantSnapStale {
				t.Fatalf("%s: snapshot staleness %v, want %v", ds.Name, ds.SnapshotsStale, wantSnapStale)
			}
			wantBackupStale := backupCadence > 0 && !ds.Paused && ds.HasRemote && ds.BackedUp > StaleFactor*backupCadence
			if ds.BackupStale != wantBackupStale {
				t.Fatalf("%s: backup staleness %v, want %v", ds.Name, ds.BackupStale, wantBackupStale)
			}

			// Fulfillment counts match the inventory.
			for _, row := range ds.Fulfillment {
				have := 0
				for snap := range local.All() {
					if snap.Type() == row.Periodicity {
						have++
					}
				}
				if row.LocalHave != have {
					t.Fatalf("%s %s: local have %d, inventory has %d", ds.Name, row.Periodicity, row.LocalHave, have)
				}
				if row.LocalWant != localPolicy[row.Periodicity] {
					t.Fatalf("%s %s: local want %d != policy", ds.Name, row.Periodicity, row.LocalWant)
				}
			}
		}
	})
}

// TestRapidIssuesIffConditions: each issue kind appears exactly when
// its condition holds, and the system verdict is a pure function of
// the issue list.
func TestRapidIssuesIffConditions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := genInput(t)
		sys := Compute(in)

		count := func(kind IssueKind, dataset model.DatasetName) int {
			n := 0
			for _, issue := range sys.Issues {
				if issue.Kind == kind && issue.Dataset != nil && *issue.Dataset == dataset {
					n++
				}
			}
			return n
		}

		for _, ds := range sys.Datasets {
			failing := ds.LastFailure != nil && (ds.LastSuccess.IsZero() || ds.LastFailure.At.After(ds.LastSuccess))
			for kind, want := range map[IssueKind]bool{
				IssueFailing:        failing,
				IssueUnreplicated:   ds.Unreplicated,
				IssueStaleBackup:    ds.BackupStale,
				IssueStaleSnapshots: ds.SnapshotsStale,
			} {
				got := count(kind, ds.Name)
				if want && got != 1 {
					t.Fatalf("%s: expected exactly one %s issue, got %d", ds.Name, kind, got)
				}
				if !want && got != 0 {
					t.Fatalf("%s: unexpected %s issue", ds.Name, kind)
				}
			}

			// Health precedence: the verdict is the highest-priority
			// triggered condition.
			var want Health
			switch {
			case failing:
				want = HealthFailing
			case ds.Unreplicated || ds.BackupStale || ds.SnapshotsStale:
				want = HealthAtRisk
			case ds.Paused:
				want = HealthPaused
			case ds.PendingDeletions+ds.PendingTransfers > 0:
				want = HealthBehind
			default:
				want = HealthOK
			}
			if ds.Health != want {
				t.Fatalf("%s: health %s, want %s", ds.Name, ds.Health, want)
			}
		}

		// Issue ordering: severities never increase.
		for i := 1; i < len(sys.Issues); i++ {
			if sys.Issues[i].Severity > sys.Issues[i-1].Severity {
				t.Fatalf("issues out of severity order at %d", i)
			}
		}

		// The system verdict follows from the issues.
		worst := Info
		alarming := false
		for _, issue := range sys.Issues {
			if issue.Severity > worst {
				worst = issue.Severity
			}
			if issue.Severity != Info {
				alarming = true
			}
		}
		var want Verdict
		switch {
		case in.Conf.Paused:
			want = SystemPaused
		case worst == Critical:
			want = SystemFailing
		case worst == Warning:
			want = SystemAttention
		default:
			want = SystemOK
		}
		if sys.Verdict != want {
			t.Fatalf("verdict %s, want %s (issues %+v)", sys.Verdict, want, sys.Issues)
		}
		if (sys.Reason == "") == (alarming || in.Conf.Paused) {
			t.Fatalf("reason %q inconsistent with verdict %s", sys.Reason, sys.Verdict)
		}
	})
}

// TestRapidQueueAccounting: cycle progress is pure arithmetic over the
// queue.
func TestRapidQueueAccounting(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		states := rapid.SliceOfN(rapid.SampledFrom([]status.QueueState{
			status.QueueWaiting, status.QueueActive, status.QueueDone,
			status.QueueFailed, status.QueueSkipped,
		}), 0, 10).Draw(t, "states")
		queue := make([]status.QueueEntry, len(states))
		active := 0
		for i, st := range states {
			queue[i] = status.QueueEntry{Dataset: model.DatasetName(fmt.Sprintf("/ds%d", i)), State: st}
			if st == status.QueueActive {
				active++
			}
		}
		c := cycleProgress(status.Activity{Queue: queue})
		if c.Total != len(queue) {
			t.Fatalf("total %d != %d", c.Total, len(queue))
		}
		done, failed, skipped, waiting := 0, 0, 0, 0
		for _, e := range queue {
			switch e.State {
			case status.QueueDone:
				done++
			case status.QueueFailed:
				failed++
			case status.QueueSkipped:
				skipped++
			case status.QueueWaiting:
				waiting++
			}
		}
		if c.Done != done || c.Failed != failed || c.Skipped != skipped {
			t.Fatalf("counts %+v disagree with queue", c)
		}
		if c.HasActive != (active > 0) {
			t.Fatalf("HasActive %v with %d active", c.HasActive, active)
		}
		wantPos := done + failed + skipped
		if c.HasActive {
			wantPos++
		}
		if c.Position != wantPos {
			t.Fatalf("position %d, want %d", c.Position, wantPos)
		}
		if c.Total-done-failed-skipped != waiting+active {
			t.Fatalf("accounting leak")
		}
	})
}
