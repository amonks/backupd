package view

import (
	"fmt"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

// The property tests treat the derivation layer as a specification: for
// arbitrary inventories, policies, histories, and activities, every
// claim the dashboard makes must follow from the state it was derived
// from — ages come from the inventory, issues appear exactly when
// their condition holds, and the system verdict is a pure function of
// the issue list.

var snapshotTypes = []string{"hourly", "daily", "weekly", "monthly"}

// genSnaps generates a set of snapshots for one dataset with ages up
// to ~80 hours before now.
func genSnaps(tc hegel.TestCase, dataset model.DatasetName) []*model.Snapshot {
	n := hegel.Draw(tc, hegel.Integers(0, 12))
	byID := map[string]*model.Snapshot{}
	for range n {
		typ := hegel.Draw(tc, hegel.SampledFrom(snapshotTypes))
		hour := hegel.Draw(tc, hegel.Integers(0, 80))
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

func genPolicy(tc hegel.TestCase) map[string]int {
	policy := map[string]int{}
	for _, typ := range snapshotTypes {
		if hegel.Draw(tc, hegel.Booleans()) {
			policy[typ] = hegel.Draw(tc, hegel.Integers(0, 5))
		}
	}
	return policy
}

func genConf(tc hegel.TestCase) *config.Config {
	conf := &config.Config{}
	conf.Local.Policy = genPolicy(tc)
	conf.Remote.Policy = genPolicy(tc)
	conf.Paused = hegel.Draw(tc, hegel.Booleans())
	if hegel.Draw(tc, hegel.Booleans()) {
		conf.SnitchID = "snitch"
	}
	return conf
}

func genInput(tc hegel.TestCase) Input {
	names := []model.DatasetName{"/a", "/b", "/c"}
	// Zero datasets is a model nobody has read yet: a listing always
	// names the root.
	count := hegel.Draw(tc, hegel.Integers(0, 3))
	state := model.New()
	hist := history.New()
	for _, name := range names[:count] {
		// Either side may be unread — the minute after a start while
		// the remote is listed, or a start with it unreachable — but a
		// dataset is in the model because some side was read.
		readLocal := hegel.Draw(tc, hegel.Booleans())
		readRemote := !readLocal || hegel.Draw(tc, hegel.Booleans())
		if readLocal {
			local := genSnaps(tc, name)
			state = model.AddLocalDataset(name, local, nil)(state)
		}
		if readRemote {
			remote := genSnaps(tc, name)
			state = model.AddRemoteDataset(name, remote, nil)(state)
		}

		// Arbitrary run history: maybe a success, maybe a failure, in
		// either order.
		if hegel.Draw(tc, hegel.Booleans()) {
			at := now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 300))) * time.Minute)
			hist.RecordDatasetSuccess(name, at)
		}
		if hegel.Draw(tc, hegel.Booleans()) {
			at := now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 300))) * time.Minute)
			hist.RecordDatasetFailure(name, at, "induced failure")
		}
	}

	phase := hegel.Draw(tc, hegel.SampledFrom([]status.Phase{
		status.Starting, status.Refreshing, status.Syncing, status.Idle, status.BackingOff,
	}))
	activity := status.Activity{Phase: phase}
	if phase == status.Syncing && count > 0 {
		activity.Dataset = hegel.Draw(tc, hegel.SampledFrom(names[:count]))
	}
	if phase == status.BackingOff {
		activity.ConsecutiveFailures = hegel.Draw(tc, hegel.Integers(1, 5))
	}

	if hegel.Draw(tc, hegel.Booleans()) {
		hist.RecordCycle(history.Cycle{
			StartedAt: now.Add(-time.Hour),
			StoppedAt: now.Add(-30 * time.Minute),
			OK:        hegel.Draw(tc, hegel.Booleans()),
			Error:     "cycle error",
		})
	}
	if hegel.Draw(tc, hegel.Booleans()) {
		hist.RecordSnitch(now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 600))) * time.Minute))
	}

	return Input{
		State:    state,
		Conf:     genConf(tc),
		History:  hist,
		Activity: activity,
		Now:      now,
		Boot:     now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 600))) * time.Minute),
	}
}

// TestAssuranceFromInventory: every age the dashboard shows is
// exactly the inventory's answer — restart-proof ground truth.
func TestAssuranceFromInventory(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		in := genInput(t)
		sys := Compute(in)

		if len(sys.Datasets) != len(in.State.ListDatasets()) {
			t.Fatalf("expected every dataset to appear exactly once: %d datasets in the view, %d in the model",
				len(sys.Datasets), len(in.State.ListDatasets()))
		}
		for _, ds := range sys.Datasets {
			m := in.State.GetDataset(ds.Name)
			local, remote := m.Current.Local, m.Current.Remote

			if ds.LocalKnown != m.LocalKnown || ds.RemoteKnown != m.RemoteKnown {
				t.Fatalf("%s: known flags local %v remote %v, the model's local %v remote %v",
					ds.Name, ds.LocalKnown, ds.RemoteKnown, m.LocalKnown, m.RemoteKnown)
			}
			if ds.HasLocal != (m.LocalKnown && local.Len() > 0) || ds.HasRemote != (m.RemoteKnown && remote.Len() > 0) {
				t.Fatalf("%s: presence flags local %v remote %v disagree with inventory (known %v/%v, %d local, %d remote)",
					ds.Name, ds.HasLocal, ds.HasRemote, m.LocalKnown, m.RemoteKnown, local.Len(), remote.Len())
			}
			if ds.HasLocal && ds.Snapshotted != in.Now.Sub(local.Newest().Time()) {
				t.Fatalf("%s: snapshotted %s != newest local age %s", ds.Name, ds.Snapshotted, in.Now.Sub(local.Newest().Time()))
			}
			if ds.HasRemote {
				if ds.BackedUp != in.Now.Sub(remote.Newest().Time()) {
					t.Fatalf("%s: backed up %s != newest remote age %s", ds.Name, ds.BackedUp, in.Now.Sub(remote.Newest().Time()))
				}
				if ds.Depth != in.Now.Sub(remote.Oldest().Time()) {
					t.Fatalf("%s: depth %s != oldest remote age %s", ds.Name, ds.Depth, in.Now.Sub(remote.Oldest().Time()))
				}
			}
			if ds.HasLocal && ds.HasRemote {
				if ds.Lag != ds.BackedUp-ds.Snapshotted {
					t.Fatalf("%s: lag %s != backed up %s - snapshotted %s", ds.Name, ds.Lag, ds.BackedUp, ds.Snapshotted)
				}
			}
			// Absence is asserted only of a side that has been read.
			if ds.Unreplicated != (ds.HasLocal && ds.RemoteKnown && !ds.HasRemote) {
				t.Fatalf("%s: unreplicated %v with local present %v, remote known %v, remote present %v",
					ds.Name, ds.Unreplicated, ds.HasLocal, ds.RemoteKnown, ds.HasRemote)
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
			wantSnapStale := cadence > 0 && ds.LocalKnown && (!ds.HasLocal || ds.Snapshotted > StaleFactor*cadence)
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

// TestIssuesIffConditions: each issue kind appears exactly when
// its condition holds, and the system verdict is a pure function of
// the issue list.
func TestIssuesIffConditions(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
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
					t.Fatalf("%s: unexpected %s issue (%d of them)", ds.Name, kind, got)
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
			case !ds.LocalKnown || !ds.RemoteKnown:
				want = HealthUnknown
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
				t.Fatalf("issues out of severity order at %d: %s after %s", i, sys.Issues[i].Severity, sys.Issues[i-1].Severity)
			}
		}

		worst := Info
		for _, issue := range sys.Issues {
			if issue.Severity > worst {
				worst = issue.Severity
			}
		}
		unknown := 0
		for _, ds := range sys.Datasets {
			if !ds.LocalKnown || !ds.RemoteKnown {
				unknown++
			}
		}
		if sys.Unknown != unknown {
			t.Fatalf("unknown count %d, want %d", sys.Unknown, unknown)
		}

		// The system verdict follows from the issues, and from what has
		// not been read when nothing alarms: a bad thing that is true
		// outranks not knowing, and not knowing outranks all clear.
		var want Verdict
		switch {
		case in.Conf.Paused:
			want = SystemPaused
		case worst == Critical:
			want = SystemFailing
		case worst == Warning:
			want = SystemAttention
		case unknown > 0 || len(sys.Datasets) == 0:
			want = SystemUnknown
		default:
			want = SystemOK
		}
		if sys.Verdict != want {
			t.Fatalf("verdict %s, want %s (issues %+v)", sys.Verdict, want, sys.Issues)
		}
		if (sys.Reason == "") != (sys.Verdict == SystemOK) {
			t.Fatalf("reason %q inconsistent with verdict %s", sys.Reason, sys.Verdict)
		}
	})
}

// TestQueueAccounting: cycle progress is pure arithmetic over the
// queue.
func TestQueueAccounting(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		states := hegel.Draw(t, hegel.Lists(hegel.SampledFrom([]status.QueueState{
			status.QueueWaiting, status.QueueActive, status.QueueDone,
			status.QueueFailed, status.QueueSkipped,
		})).MaxSize(10))
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
			t.Fatalf("counts %+v disagree with queue %v: want done %d, failed %d, skipped %d", c, states, done, failed, skipped)
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
			t.Fatalf("accounting leak: total %d - done %d - failed %d - skipped %d != waiting %d + active %d",
				c.Total, done, failed, skipped, waiting, active)
		}
	})
}
