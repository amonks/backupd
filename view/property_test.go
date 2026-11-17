package view

import (
	"fmt"
	"maps"
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
// their condition holds, and the system verdict follows from which
// conditions hold. What each claim should be is derived here from the
// inputs (expect), by this file's own reading of the spec's rules:
// never from another of the view's fields, nor from the config's own
// resolution of a dataset's policies and pause.

// snapshotTypes are the periodicities generated snapshots and policies
// use, shortest first; periodOf is each one's nominal period.
var snapshotTypes = []string{"hourly", "daily", "weekly", "monthly"}

var periodOf = map[string]time.Duration{
	"hourly":  time.Hour,
	"daily":   24 * time.Hour,
	"weekly":  7 * 24 * time.Hour,
	"monthly": 30 * 24 * time.Hour,
}

// cadenceOf applies the spec's cadence rule (§ Dashboard): the smallest
// periodicity a policy retains, zero for none.
func cadenceOf(policy map[string]int) time.Duration {
	for _, typ := range snapshotTypes {
		if policy[typ] > 0 {
			return periodOf[typ]
		}
	}
	return 0
}

// severityOf is the spec's severity for each issue kind (§ Dashboard,
// issues).
var severityOf = map[IssueKind]Severity{
	IssueFailing:        Critical,
	IssueUnreplicated:   Critical,
	IssueStaleBackup:    Critical,
	IssueCycleFailing:   Critical,
	IssueStaleSnapshots: Warning,
	IssueSnitchOverdue:  Warning,
	IssuePaused:         Info,
}

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

// genOverride draws an override, paused as asked, naming a policy of
// its own on each side half the time. A side it names replaces the
// global policy wholesale — an empty policy included, which expects no
// cadence at all — and a side it leaves out falls back to the global
// one (§ Per-Dataset Overrides).
func genOverride(tc hegel.TestCase, paused bool) *config.Override {
	o := &config.Override{Paused: paused}
	if hegel.Draw(tc, hegel.Booleans()) {
		o.Local.Policy = genPolicy(tc)
	}
	if hegel.Draw(tc, hegel.Booleans()) {
		o.Remote.Policy = genPolicy(tc)
	}
	return o
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

// within reports whether the dataset at path is key's or under it: the
// datasets an override at key reaches.
func within(path, key string) bool {
	return path == key || strings.HasPrefix(path, key+"/")
}

// subtreePaused applies the spec's pause rule (§ Pause) from the
// config: pause is cumulative, so a dataset is paused by its own key
// or any ancestor's.
func subtreePaused(conf *config.Config, name model.DatasetName) bool {
	for key := range pausedKeys(conf) {
		if within(name.Path(), key) {
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

// policiesOf applies the spec's retention rule (§ Per-Dataset
// Overrides) from the config: the override at the longest key over the
// dataset wins, and gives its policy on each side it names one, the
// global policy on a side it leaves out. overridden says whether any
// override reaches the dataset.
func policiesOf(conf *config.Config, name model.DatasetName) (local, remote map[string]int, overridden bool) {
	var best string
	var override *config.Override
	for key, o := range conf.Overrides {
		if within(name.Path(), key) && (override == nil || len(key) > len(best)) {
			best, override = key, o
		}
	}
	local, remote = conf.Local.Policy, conf.Remote.Policy
	if override == nil {
		return local, remote, false
	}
	if override.Local.Policy != nil {
		local = override.Local.Policy
	}
	if override.Remote.Policy != nil {
		remote = override.Remote.Policy
	}
	return local, remote, true
}

// expected is what § Dashboard says the view shows of one dataset,
// derived here from the inputs alone: the model's inventory and plan,
// the config's policies and pauses, the run history, the clock.
type expected struct {
	localKnown, remoteKnown bool
	// local and remote are the read sides' inventories, nil for a side
	// not read: nothing is asserted of it.
	local, remote             *model.Snapshots
	hasLocal, hasRemote       bool
	snapshotted, backedUp     time.Duration
	depth                     time.Duration
	localPolicy, remotePolicy map[string]int
	overridden                bool
	cadence, backupCadence    time.Duration
	snapshotsStale            bool
	backupStale, unreplicated bool
	globalPaused, subtree     bool
	failure                   history.Failure
	failing                   bool
	work                      work
	health                    Health
}

func expect(t *hegel.T, in Input, name model.DatasetName) expected {
	m := in.State.GetDataset(name)
	e := expected{localKnown: m.LocalKnown, remoteKnown: m.RemoteKnown}
	if e.localKnown {
		e.local = m.Current.Local
		e.hasLocal = e.local.Len() > 0
	}
	if e.remoteKnown {
		e.remote = m.Current.Remote
		e.hasRemote = e.remote.Len() > 0
	}
	if e.hasLocal {
		e.snapshotted = in.Now.Sub(e.local.Newest().Time())
	}
	if e.hasRemote {
		e.backedUp = in.Now.Sub(e.remote.Newest().Time())
		e.depth = in.Now.Sub(e.remote.Oldest().Time())
	}

	// Staleness follows the cadence rule: snapshots against the local
	// policy's cadence, the backup against the remote policy's, each
	// the dataset's own.
	e.localPolicy, e.remotePolicy, e.overridden = policiesOf(in.Conf, name)
	e.cadence, e.backupCadence = cadenceOf(e.localPolicy), cadenceOf(e.remotePolicy)
	e.globalPaused, e.subtree = in.Conf.Paused, subtreePaused(in.Conf, name)
	paused := e.globalPaused || e.subtree
	e.snapshotsStale = e.cadence > 0 && e.localKnown && (!e.hasLocal || e.snapshotted > StaleFactor*e.cadence)
	e.backupStale = e.backupCadence > 0 && !paused && e.hasRemote && e.backedUp > StaleFactor*e.backupCadence
	// Absence is asserted only of a side that has been read.
	e.unreplicated = e.hasLocal && e.remoteKnown && !e.hasRemote

	e.failure, e.failing = failingIn(in.History, name)
	e.work = workIn(t, m)

	// Health precedence (§ Dashboard, dataset health).
	switch {
	case e.failing:
		e.health = HealthFailing
	case e.unreplicated || e.backupStale || e.snapshotsStale:
		e.health = HealthAtRisk
	case !e.localKnown || !e.remoteKnown:
		e.health = HealthUnknown
	case paused:
		e.health = HealthPaused
	case e.work.deletions+e.work.transfers > 0:
		e.health = HealthBehind
	default:
		e.health = HealthOK
	}
	return e
}

// cycleFailingIn applies the spec's cycle rule (§ Dashboard, issues):
// the daemon is backing off, or the most recent cycle failed.
func cycleFailingIn(in Input) bool {
	cycles := in.History.Cycles()
	return in.Activity.Phase == status.BackingOff || len(cycles) > 0 && !cycles[0].OK
}

// snitchOverdueIn applies the spec's snitch rule (§ Dashboard,
// issues): a snitch is configured, execution isn't paused, and no ping
// has succeeded — since boot, if none ever has — within
// SnitchOverdueFactor intervals.
func snitchOverdueIn(in Input) bool {
	if in.Conf.SnitchID == "" || in.Conf.Paused {
		return false
	}
	ref, pinged := in.History.LastSnitch()
	if !pinged {
		ref = in.Boot
	}
	return !ref.IsZero() && in.Now.Sub(ref) > SnitchOverdueFactor*in.Conf.Interval()
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
// accumulates, where retention's longest prefix picks the dataset's
// policies, which an override names of its own half the time a side
// (genOverride).
const overrideRate = 0.35

// nestRate is how often a case draws, outright, the one shape on which
// those two rules disagree: a paused key, and an unpaused one under it
// on the way to a dataset in the model (genInput).
const nestRate = 0.2

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

	conf.Overrides = map[string]*config.Override{}
	for _, key := range datasetPool {
		if hegel.Draw(tc, hegel.WeightedBooleans(overrideRate)) {
			paused := hegel.Draw(tc, hegel.WeightedBooleans(pauseRate/overrideRate))
			conf.Overrides[key.Path()] = genOverride(tc, paused)
		}
	}
	// Drawn key by key, the overrides meet a paused key with an unpaused
	// one nearer a dataset in about one case in twenty, and unevenly: a
	// default run of a hundred holds anywhere from none to a score, and
	// one run in twenty to one in a hundred would pass a pause resolved
	// by longest prefix, as retention is. So a case also draws that shape
	// outright, at nestRate, on the way to one of its datasets; a default
	// run then holds ten to thirty-five.
	if hegel.Draw(tc, hegel.WeightedBooleans(nestRate)) {
		var nests [][2]string
		for _, name := range names {
			for _, outer := range datasetPool {
				for _, inner := range datasetPool {
					if inner != outer && within(inner.Path(), outer.Path()) && within(name.Path(), inner.Path()) {
						nests = append(nests, [2]string{outer.Path(), inner.Path()})
					}
				}
			}
		}
		if len(nests) > 0 {
			nest := hegel.Draw(tc, hegel.SampledFrom(nests))
			conf.Overrides[nest[0]] = genOverride(tc, true)
			conf.Overrides[nest[1]] = genOverride(tc, false)
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
// exactly the inventory's answer — restart-proof ground truth — and
// every other fact about a dataset is what its inputs say: its
// cadences its own policies', its work the plan's, its run record the
// history's, its pause the config's.
func TestAssuranceFromInventory(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		in := genInput(t)
		sys := Compute(in)

		names := in.State.ListDatasets()
		if len(sys.Datasets) != len(names) {
			t.Fatalf("expected every dataset to appear exactly once: %d datasets in the view, %d in the model",
				len(sys.Datasets), len(names))
		}
		for _, name := range names {
			ds, ok := sys.Get(name)
			if !ok {
				t.Fatalf("%s is in the model but not the view", name)
			}
			e := expect(t, in, name)

			if ds.LocalKnown != e.localKnown || ds.RemoteKnown != e.remoteKnown {
				t.Fatalf("%s: known flags local %v remote %v, the model's local %v remote %v",
					name, ds.LocalKnown, ds.RemoteKnown, e.localKnown, e.remoteKnown)
			}
			if ds.HasLocal != e.hasLocal || ds.HasRemote != e.hasRemote {
				t.Fatalf("%s: presence flags local %v remote %v, the inventory's %v/%v (known %v/%v)",
					name, ds.HasLocal, ds.HasRemote, e.hasLocal, e.hasRemote, e.localKnown, e.remoteKnown)
			}
			if e.hasLocal && ds.Snapshotted != e.snapshotted {
				t.Fatalf("%s: snapshotted %s != newest local age %s", name, ds.Snapshotted, e.snapshotted)
			}
			if e.hasRemote && (ds.BackedUp != e.backedUp || ds.Depth != e.depth) {
				t.Fatalf("%s: backed up %s, depth %s; newest remote age %s, oldest %s",
					name, ds.BackedUp, ds.Depth, e.backedUp, e.depth)
			}
			if e.hasLocal && e.hasRemote && ds.Lag != e.backedUp-e.snapshotted {
				t.Fatalf("%s: lag %s, the newest remote's age less the newest local's is %s", name, ds.Lag, e.backedUp-e.snapshotted)
			}
			if ds.Unreplicated != e.unreplicated {
				t.Fatalf("%s: unreplicated %v, want %v (local present %v, remote known %v, remote present %v)",
					name, ds.Unreplicated, e.unreplicated, e.hasLocal, e.remoteKnown, e.hasRemote)
			}

			if ds.Overridden != e.overridden {
				t.Fatalf("%s: overridden %v, want %v", name, ds.Overridden, e.overridden)
			}
			if ds.Cadence != e.cadence || ds.BackupCadence != e.backupCadence {
				t.Fatalf("%s: cadences %s/%s != its policies' cadences %s/%s (local %v, remote %v)",
					name, ds.Cadence, ds.BackupCadence, e.cadence, e.backupCadence, e.localPolicy, e.remotePolicy)
			}
			if ds.SnapshotsStale != e.snapshotsStale {
				t.Fatalf("%s: snapshot staleness %v, want %v", name, ds.SnapshotsStale, e.snapshotsStale)
			}
			if ds.BackupStale != e.backupStale {
				t.Fatalf("%s: backup staleness %v, want %v", name, ds.BackupStale, e.backupStale)
			}

			// Fulfillment: a row for each periodicity a policy wants or a
			// read side holds, its counts the policies' and the
			// inventory's.
			have := func(snaps *model.Snapshots) map[string]int {
				out := map[string]int{}
				if snaps != nil {
					for snap := range snaps.All() {
						out[snap.Type()]++
					}
				}
				return out
			}
			localHave, remoteHave := have(e.local), have(e.remote)
			rows := map[string]Fulfillment{}
			for _, row := range ds.Fulfillment {
				rows[row.Periodicity] = row
			}
			for _, counts := range []map[string]int{e.localPolicy, e.remotePolicy, localHave, remoteHave} {
				for typ, n := range counts {
					if _, ok := rows[typ]; n > 0 && !ok {
						t.Fatalf("%s: no fulfillment row for %s, which a policy wants or a side holds", name, typ)
					}
				}
			}
			for typ, row := range rows {
				want := Fulfillment{typ, e.localPolicy[typ], localHave[typ], e.remotePolicy[typ], remoteHave[typ]}
				if row != want {
					t.Fatalf("%s %s: fulfillment %+v, want %+v", name, typ, row, want)
				}
			}

			// Work comes from the plan.
			w := e.work
			if ds.PendingDeletions != w.deletions || ds.PendingTransfers != w.transfers ||
				ds.StepsDone != w.done || ds.StepsTotal != w.total {
				t.Fatalf("%s: %d deletions, %d transfers queued, %d of %d steps done; the plan has %d, %d, %d of %d",
					name, ds.PendingDeletions, ds.PendingTransfers, ds.StepsDone, ds.StepsTotal,
					w.deletions, w.transfers, w.done, w.total)
			}

			// The run record comes from the history.
			success, succeeded := in.History.LastSuccess(name)
			if !succeeded {
				success = time.Time{}
			}
			if !ds.LastSuccess.Equal(success) {
				t.Fatalf("%s: last success %v, the history's %v", name, ds.LastSuccess, success)
			}
			if f, failed := in.History.LastFailure(name); failed != (ds.LastFailure != nil) || failed && *ds.LastFailure != f {
				t.Fatalf("%s: last failure %+v, the history's %+v (on record %v)", name, ds.LastFailure, f, failed)
			}

			// Pause comes from the config.
			if ds.GlobalPaused != e.globalPaused || ds.SubtreePaused != e.subtree || ds.Paused != (e.globalPaused || e.subtree) {
				t.Fatalf("%s: paused %v (global %v, subtree %v), config says global %v, subtree %v",
					name, ds.Paused, ds.GlobalPaused, ds.SubtreePaused, e.globalPaused, e.subtree)
			}
		}
	})
}

// TestIssuesIffConditions: each issue kind appears exactly when its
// condition holds of the inputs, as severe as the spec says, and the
// system verdict follows from which those are.
func TestIssuesIffConditions(t *testing.T) {
	hegel.Test(t, func(t *hegel.T) {
		in := genInput(t)
		sys := Compute(in)

		// The issues the inputs call for, by kind and dataset ("" for
		// the daemon's own), each exactly once.
		key := func(kind IssueKind, dataset *model.DatasetName) string {
			if dataset == nil {
				return string(kind)
			}
			return string(kind) + " " + dataset.Path()
		}
		want := map[string]IssueKind{}
		raise := func(kind IssueKind, dataset *model.DatasetName, when bool) {
			if when {
				want[key(kind, dataset)] = kind
			}
		}
		unknown := 0
		for _, name := range in.State.ListDatasets() {
			e := expect(t, in, name)
			ds, ok := sys.Get(name)
			if !ok {
				t.Fatalf("%s is in the model but not the view", name)
			}
			raise(IssueFailing, &name, e.failing)
			raise(IssueUnreplicated, &name, e.unreplicated)
			raise(IssueStaleBackup, &name, e.backupStale)
			raise(IssueStaleSnapshots, &name, e.snapshotsStale)
			if !e.localKnown || !e.remoteKnown {
				unknown++
			}

			// Health precedence: the verdict is the highest-priority
			// triggered condition.
			if ds.Health != e.health {
				t.Fatalf("%s: health %s, want %s", name, ds.Health, e.health)
			}
			// Failing says what failed and since when.
			if e.failing && ds.Reason != e.failure.Error {
				t.Fatalf("%s: failing for %q; the history's failure is %q", name, ds.Reason, e.failure.Error)
			}
			for _, issue := range sys.Issues {
				if e.failing && issue.Kind == IssueFailing && issue.Dataset != nil && *issue.Dataset == name &&
					(issue.Detail != e.failure.Error || !issue.Since.Equal(e.failure.At)) {
					t.Fatalf("%s: failing issue says %q since %v; the history's failure is %q at %v",
						name, issue.Detail, issue.Since, e.failure.Error, e.failure.At)
				}
			}
		}
		// The daemon's own: the cycle, the snitch, and a pause reminder
		// for a global pause and for each paused subtree.
		raise(IssueCycleFailing, nil, cycleFailingIn(in))
		raise(IssueSnitchOverdue, nil, snitchOverdueIn(in))
		raise(IssuePaused, nil, in.Conf.Paused)
		for path := range pausedKeys(in.Conf) {
			name := model.DatasetName(path)
			raise(IssuePaused, &name, true)
		}

		got := map[string]int{}
		for _, issue := range sys.Issues {
			got[key(issue.Kind, issue.Dataset)]++
		}
		for k := range want {
			if got[k] != 1 {
				t.Fatalf("expected exactly one %s issue, got %d (issues %+v)", k, got[k], sys.Issues)
			}
		}
		for k, n := range got {
			if _, ok := want[k]; !ok {
				t.Fatalf("unexpected %s issue (%d of them); the inputs call for %v", k, n, slices.Sorted(maps.Keys(want)))
			}
		}

		// Each issue is as severe as the spec says its kind is.
		for _, issue := range sys.Issues {
			sev, ok := severityOf[issue.Kind]
			if !ok {
				t.Fatalf("an issue of a kind the oracle doesn't know: %+v", issue)
			}
			if issue.Severity != sev {
				t.Fatalf("%s issue %+v at %s, want %s", issue.Kind, issue, issue.Severity, sev)
			}
		}

		// Issue ordering: severities never increase.
		for i := 1; i < len(sys.Issues); i++ {
			if sys.Issues[i].Severity > sys.Issues[i-1].Severity {
				t.Fatalf("issues out of severity order at %d: %s after %s", i, sys.Issues[i].Severity, sys.Issues[i-1].Severity)
			}
		}

		if sys.Unknown != unknown {
			t.Fatalf("unknown count %d, want %d", sys.Unknown, unknown)
		}

		// The system verdict follows from the issues the inputs call
		// for, and from what has not been read when nothing alarms: a
		// bad thing that is true outranks not knowing, and not knowing
		// outranks all clear.
		worst := Info
		for _, kind := range want {
			worst = max(worst, severityOf[kind])
		}
		var verdict Verdict
		switch {
		case in.Conf.Paused:
			verdict = SystemPaused
		case worst == Critical:
			verdict = SystemFailing
		case worst == Warning:
			verdict = SystemAttention
		case unknown > 0 || len(in.State.ListDatasets()) == 0:
			verdict = SystemUnknown
		default:
			verdict = SystemOK
		}
		if sys.Verdict != verdict {
			t.Fatalf("verdict %s, want %s (issues %+v)", sys.Verdict, verdict, sys.Issues)
		}
		if (sys.Reason == "") != (verdict == SystemOK) {
			t.Fatalf("reason %q with verdict %s", sys.Reason, verdict)
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
