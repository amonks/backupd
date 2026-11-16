package view

import (
	"fmt"
	"slices"
	"strings"
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

// genSide generates one read side's snapshots. A calm side holds a
// snapshot taken just now, so no cadence finds it stale and it is
// never empty. Any other side is empty a quarter of the time — never
// replicated, or no snapshots at all — and otherwise genSnaps's.
func genSide(tc hegel.TestCase, dataset model.DatasetName, calm bool) []*model.Snapshot {
	if calm {
		newest := &model.Snapshot{Dataset: dataset, Name: "hourly-now", CreatedAt: now.Unix()}
		return append(genSnaps(tc, dataset), newest)
	}
	if hegel.Draw(tc, hegel.WeightedBooleans(0.25)) {
		return nil
	}
	return genSnaps(tc, dataset)
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

// genPlan draws a plan's steps, each an operation of any kind in any
// state: work is queued while a step has not completed.
func genPlan(tc hegel.TestCase, dataset model.DatasetName) *model.Plan {
	snap := &model.Snapshot{Dataset: dataset, Name: "daily-t00", CreatedAt: now.Unix()}
	ops := []model.Operation{
		&model.SnapshotDeletion{Location: model.Local, Snapshot: snap},
		&model.SnapshotDeletion{Location: model.Remote, Snapshot: snap},
		&model.SnapshotRangeDeletion{Location: model.Remote, Start: snap, End: snap},
		&model.InitialSnapshotTransfer{Snapshot: snap},
		&model.SnapshotTransfer{Snapshot: snap},
		&model.SnapshotRangeTransfer{Start: snap, End: snap},
	}
	plan := &model.Plan{}
	for range hegel.Draw(tc, hegel.Integers(1, 3)) {
		step := model.NewPlanStep(ops[hegel.Draw(tc, hegel.Integers(0, len(ops)-1))])
		step.Status = hegel.Draw(tc, hegel.SampledFrom([]model.StepStatus{
			model.StepPending, model.StepInProgress, model.StepCompleted, model.StepFailed,
		}))
		plan.Steps = append(plan.Steps, step)
	}
	return plan
}

// work is a dataset's plan as the spec describes it, counted here from
// the input rather than by the view: every step not yet completed is
// queued, a transfer or a deletion by its operation.
type work struct{ deletions, transfers, done, total int }

func workIn(t *hegel.T, m *model.Dataset) work {
	var w work
	if m == nil || m.Plan == nil {
		return w
	}
	for _, step := range m.Plan.Steps {
		w.total++
		if step.Status == model.StepCompleted {
			w.done++
			continue
		}
		switch op := step.Operation.(type) {
		case *model.SnapshotDeletion, *model.SnapshotRangeDeletion:
			w.deletions++
		case *model.InitialSnapshotTransfer, *model.SnapshotTransfer, *model.SnapshotRangeTransfer:
			w.transfers++
		default:
			t.Fatalf("generated an operation the oracle doesn't know: %T", op)
		}
	}
	return w
}

// pausedKeys are the override keys whose subtree the config pauses.
// genInput keys overrides by names from datasetPool, already in normal
// form.
func pausedKeys(conf *config.Config) map[string]bool {
	out := map[string]bool{}
	for key, override := range conf.Overrides {
		if override.Paused {
			out[key] = true
		}
	}
	return out
}

// subtreePaused applies the spec's pause rule (§ Pause) from the
// config: pause is cumulative, so a dataset is paused by its own key
// or any ancestor's.
func subtreePaused(conf *config.Config, name model.DatasetName) bool {
	for key := range pausedKeys(conf) {
		if name.Path() == key || strings.HasPrefix(name.Path(), key+"/") {
			return true
		}
	}
	return false
}

// failingIn applies the spec's failing rule (§ Dashboard, dataset
// health) to the run history: a failure is on record, and no success
// since. It answers the failure it judged.
func failingIn(hist *history.History, name model.DatasetName) (history.Failure, bool) {
	f, failed := hist.LastFailure(name)
	if !failed {
		return f, false
	}
	success, succeeded := hist.LastSuccess(name)
	return f, !succeeded || f.At.After(success)
}

// pauseRate is how often the operator has paused execution, globally or
// at one override's subtree: a choice, not trouble, so it is drawn on
// its own, and rarely enough that most cases run.
const pauseRate = 0.15

// datasetPool is the tree a model's datasets are drawn from: /a three
// deep with a sibling under it, /b beside it, and /ab, whose name
// extends /a's without being under it. Pause accumulates down the tree
// (§ Pause), so its rule is reached only by a dataset under a paused
// ancestor, and its edge only by a name like /ab.
var datasetPool = []model.DatasetName{"/a", "/a/b", "/a/b/c", "/a/c", "/ab", "/b"}

// overrideRate is how often a name in the pool carries an override,
// paused at pauseRate overall. The rest pause nothing, so a dataset's
// own unpaused override can sit under a paused ancestor's: pause
// accumulates, where retention's longest prefix picks the dataset's.
const overrideRate = 0.35

// genInput draws the dashboard's inputs, the shape first.
//
// The verdict and health branches are conjunctions across datasets
// and system-level sources: "1 of 3 datasets not read yet" needs
// several datasets, exactly one with a side unread, and nothing
// alarming anywhere; all clear needs every side read and every source
// calm. Drawn as independent coin flips, such a conjunction comes up in
// a fraction of a percent of cases, and a fault in its branch survives
// the default run. So a case first draws its trouble: the chance that
// each source able to raise an issue — a side's snapshots, a dataset's
// failure record, the backoff phase, the last cycle's outcome, the
// snitch's last ping — is drawn arbitrarily rather than calm. A third
// of the cases draw every source arbitrarily (trouble 1), a third have
// one or two things wrong amid calm (0.2), and a third have nothing
// alarming (0), leaving which sides were read, queued work, and pauses
// to vary. How many datasets are unread is likewise one draw rather
// than a coin per dataset.
func genInput(tc hegel.TestCase) Input {
	trouble := hegel.Draw(tc, hegel.SampledFrom([]float64{0, 0.2, 1}))
	stray := func() bool { return hegel.Draw(tc, hegel.WeightedBooleans(trouble)) }

	conf := &config.Config{}
	conf.Local.Policy = genPolicy(tc)
	conf.Remote.Policy = genPolicy(tc)
	conf.Paused = hegel.Draw(tc, hegel.WeightedBooleans(pauseRate))
	if hegel.Draw(tc, hegel.Booleans()) {
		conf.SnitchID = "snitch"
	}

	// Zero datasets is a model nobody has read yet: a listing always
	// names the root. The rest are drawn from the pool without
	// replacement, so a dataset can come without its parent: the view
	// derives each on its own, and an override pauses its subtree
	// whether or not the key is itself a dataset.
	count := hegel.Draw(tc, hegel.Integers(0, len(datasetPool)))
	pool := slices.Clone(datasetPool)
	names := make([]model.DatasetName, 0, count)
	for range count {
		i := hegel.Draw(tc, hegel.Integers(0, len(pool)-1))
		names = append(names, pool[i])
		pool = slices.Delete(pool, i, i+1)
	}
	// Either side may be unread — the minute after a start while the
	// remote is listed, or a start with it unreachable — but a dataset
	// is in the model because some side was read. How many datasets
	// have a side unread is one draw — the first that many of names,
	// which came in a drawn order — so that none, exactly one, and all
	// of them each come up routinely, and so is the side: a listing
	// reads one side of every dataset at once, though any mix is
	// possible.
	unread := hegel.Draw(tc, hegel.Integers(0, count))
	unreadSide := ""
	if unread != 0 {
		unreadSide = hegel.Draw(tc, hegel.SampledFrom([]string{"remote", "local", "either"}))
	}
	state := model.New()
	hist := history.New()
	for i, name := range names {
		side := "neither"
		if i < unread {
			side = unreadSide
			if side == "either" {
				side = hegel.Draw(tc, hegel.SampledFrom([]string{"remote", "local"}))
			}
		}
		if side != "local" {
			state = model.AddLocalDataset(name, genSide(tc, name, !stray()), nil)(state)
		}
		if side != "remote" {
			state = model.AddRemoteDataset(name, genSide(tc, name, !stray()), nil)(state)
		}
		if hegel.Draw(tc, hegel.Booleans()) {
			state = state.Clone()
			state.SetPlan(name, genPlan(tc, name))
		}
		// Run history: maybe a success, maybe a failure, in either
		// order. A calm dataset has no failure on record.
		if hegel.Draw(tc, hegel.Booleans()) {
			at := now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 300))) * time.Minute)
			hist.RecordDatasetSuccess(name, at)
		}
		if stray() && hegel.Draw(tc, hegel.Booleans()) {
			at := now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, 300))) * time.Minute)
			hist.RecordDatasetFailure(name, at, "induced failure on "+name.Path())
		}
	}

	for _, key := range datasetPool {
		if hegel.Draw(tc, hegel.WeightedBooleans(overrideRate)) {
			if conf.Overrides == nil {
				conf.Overrides = map[string]*config.Override{}
			}
			paused := hegel.Draw(tc, hegel.WeightedBooleans(pauseRate/overrideRate))
			conf.Overrides[key.Path()] = &config.Override{Paused: paused}
		}
	}

	phases := []status.Phase{status.Starting, status.Refreshing, status.Syncing, status.Idle}
	if stray() {
		phases = append(phases, status.BackingOff)
	}
	phase := hegel.Draw(tc, hegel.SampledFrom(phases))
	activity := status.Activity{Phase: phase}
	if phase == status.Syncing && count > 0 {
		activity.Dataset = hegel.Draw(tc, hegel.SampledFrom(names))
	}
	if phase == status.BackingOff {
		activity.ConsecutiveFailures = hegel.Draw(tc, hegel.Integers(1, 5))
	}

	if hegel.Draw(tc, hegel.Booleans()) {
		hist.RecordCycle(history.Cycle{
			StartedAt: now.Add(-time.Hour),
			StoppedAt: now.Add(-30 * time.Minute),
			OK:        !stray() || hegel.Draw(tc, hegel.Booleans()),
			Error:     "cycle error",
		})
	}
	// A calm snitch pinged, or the daemon booted, within the last sync
	// interval.
	maxAge := int(conf.Interval() / time.Minute)
	if stray() {
		maxAge = 600
	}
	if hegel.Draw(tc, hegel.Booleans()) {
		hist.RecordSnitch(now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, maxAge))) * time.Minute))
	}

	return Input{
		State:    state,
		Conf:     conf,
		History:  hist,
		Activity: activity,
		Now:      now,
		Boot:     now.Add(-time.Duration(hegel.Draw(tc, hegel.Integers(0, maxAge))) * time.Minute),
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
			paused := in.Conf.Paused || subtreePaused(in.Conf, ds.Name)
			wantBackupStale := backupCadence > 0 && !paused && ds.HasRemote && ds.BackedUp > StaleFactor*backupCadence
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

			// Work comes from the plan.
			w := workIn(t, m)
			if ds.PendingDeletions != w.deletions || ds.PendingTransfers != w.transfers ||
				ds.StepsDone != w.done || ds.StepsTotal != w.total {
				t.Fatalf("%s: %d deletions, %d transfers queued, %d of %d steps done; the plan has %d, %d, %d of %d",
					ds.Name, ds.PendingDeletions, ds.PendingTransfers, ds.StepsDone, ds.StepsTotal,
					w.deletions, w.transfers, w.done, w.total)
			}

			// The run record comes from the history.
			success, succeeded := in.History.LastSuccess(ds.Name)
			if !succeeded {
				success = time.Time{}
			}
			if !ds.LastSuccess.Equal(success) {
				t.Fatalf("%s: last success %v, the history's %v", ds.Name, ds.LastSuccess, success)
			}
			if f, failed := in.History.LastFailure(ds.Name); failed != (ds.LastFailure != nil) || failed && *ds.LastFailure != f {
				t.Fatalf("%s: last failure %+v, the history's %+v (on record %v)", ds.Name, ds.LastFailure, f, failed)
			}

			// Pause comes from the config.
			subtree := subtreePaused(in.Conf, ds.Name)
			if ds.GlobalPaused != in.Conf.Paused || ds.SubtreePaused != subtree || ds.Paused != (in.Conf.Paused || subtree) {
				t.Fatalf("%s: paused %v (global %v, subtree %v), config says global %v, subtree %v",
					ds.Name, ds.Paused, ds.GlobalPaused, ds.SubtreePaused, in.Conf.Paused, subtree)
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

		find := func(kind IssueKind, dataset model.DatasetName) []Issue {
			var out []Issue
			for _, issue := range sys.Issues {
				if issue.Kind == kind && issue.Dataset != nil && *issue.Dataset == dataset {
					out = append(out, issue)
				}
			}
			return out
		}
		count := func(kind IssueKind, dataset model.DatasetName) int { return len(find(kind, dataset)) }

		for _, ds := range sys.Datasets {
			// Failing comes from the history, and says what failed and
			// since when.
			failure, failing := failingIn(in.History, ds.Name)
			if failing {
				issue := find(IssueFailing, ds.Name)
				if len(issue) == 1 && (issue[0].Detail != failure.Error || !issue[0].Since.Equal(failure.At)) {
					t.Fatalf("%s: failing issue says %q since %v; the history's failure is %q at %v",
						ds.Name, issue[0].Detail, issue[0].Since, failure.Error, failure.At)
				}
				if ds.Reason != failure.Error {
					t.Fatalf("%s: failing for %q; the history's failure is %q", ds.Name, ds.Reason, failure.Error)
				}
			}
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
			// triggered condition, pause read from the config and work
			// from the plan.
			w := workIn(t, in.State.GetDataset(ds.Name))
			var want Health
			switch {
			case failing:
				want = HealthFailing
			case ds.Unreplicated || ds.BackupStale || ds.SnapshotsStale:
				want = HealthAtRisk
			case !ds.LocalKnown || !ds.RemoteKnown:
				want = HealthUnknown
			case in.Conf.Paused || subtreePaused(in.Conf, ds.Name):
				want = HealthPaused
			case w.deletions+w.transfers > 0:
				want = HealthBehind
			default:
				want = HealthOK
			}
			if ds.Health != want {
				t.Fatalf("%s: health %s, want %s", ds.Name, ds.Health, want)
			}
		}

		// Pause reminders come from the config: one for a global pause
		// and one for each paused subtree, every one at info.
		unlisted := pausedKeys(in.Conf)
		global := 0
		for _, issue := range sys.Issues {
			if issue.Kind != IssuePaused {
				continue
			}
			if issue.Severity != Info {
				t.Fatalf("pause reminder %+v at %s, want info", issue, issue.Severity)
			}
			if issue.Dataset == nil {
				global++
				continue
			}
			if !unlisted[issue.Dataset.Path()] {
				t.Fatalf("pause reminder for %s, which no paused override names or which is listed twice", issue.Dataset)
			}
			delete(unlisted, issue.Dataset.Path())
		}
		if len(unlisted) > 0 {
			t.Fatalf("no pause reminder for the paused subtrees %v", unlisted)
		}
		wantGlobal := 0
		if in.Conf.Paused {
			wantGlobal = 1
		}
		if global != wantGlobal {
			t.Fatalf("%d global pause reminders, want %d", global, wantGlobal)
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
