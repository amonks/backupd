// Package view derives what the dashboard says from what the system
// knows. It is the single judgment layer between raw state — the
// model rebuilt from ZFS each cycle, the config, the run history, the
// live activity — and every surface that makes claims about the
// system: the HTML pages, the status strip, and /api/state. Keeping
// the derivations pure (state in, verdicts out, the clock passed
// explicitly) makes every judgment call testable, and having exactly
// one derivation layer means the HTML and the JSON cannot disagree.
//
// Two principles govern the derivations:
//
// Assurance comes from ground truth. Any claim that data is safe —
// how recently a dataset was snapshotted or backed up, how far back
// history reaches — is computed from the snapshot inventory and the
// clock, never from run history. The inventory is queried from ZFS
// and survives restarts; history is in-memory and empty at boot.
// History answers only "what did the daemon do" (run outcomes, cycle
// records).
//
// Health and activity are orthogonal. A dataset's health (failing,
// at risk, paused, behind, ok) describes whether its promise is being
// kept; whether it is being synced right now is a separate activity
// flag. Folding "syncing" into health would hide a failing dataset's
// red flag at exactly the moment the operator watches it retry.
package view

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"monks.co/backupd/config"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/status"
)

// StaleFactor is the grace multiplier applied to a dataset's cadence
// (the smallest periodicity in its local policy) before its snapshots
// or backup count as stale: one full period late — a missed cron run —
// is tolerated, two is an alarm.
const StaleFactor = 2

// SnitchOverdueFactor is the grace multiplier applied to the sync
// interval before a missing snitch ping becomes an issue.
const SnitchOverdueFactor = 2

// Health is a dataset's verdict: is its promise being kept?
type Health int

const (
	HealthUnknown Health = iota
	HealthOK
	// HealthBehind: work is queued. Routine between snapshot arrivals,
	// so it never raises an issue; staleness is what makes a backlog a
	// problem.
	HealthBehind
	// HealthPaused: execution intentionally stopped for this dataset.
	HealthPaused
	// HealthAtRisk: the data-safety promise is not being kept — never
	// replicated, backup too old, or snapshots not being taken.
	HealthAtRisk
	// HealthFailing: the most recent sync attempt errored.
	HealthFailing
)

// String is the operator-facing label.
func (h Health) String() string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthBehind:
		return "behind"
	case HealthPaused:
		return "paused"
	case HealthAtRisk:
		return "at risk"
	case HealthFailing:
		return "failing"
	default:
		return "unknown"
	}
}

// Class is the CSS class suffix for chips and dots.
func (h Health) Class() string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthBehind:
		return "pending"
	case HealthPaused:
		return "paused"
	case HealthAtRisk:
		return "atrisk"
	case HealthFailing:
		return "failing"
	default:
		return "unknown"
	}
}

// Severity ranks issues: Info is intentional state worth a reminder,
// Warning is degraded, Critical means data is at risk or not being
// backed up.
type Severity int

const (
	Info Severity = iota
	Warning
	Critical
)

func (s Severity) String() string {
	switch s {
	case Critical:
		return "critical"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// IssueKind identifies the condition an issue reports.
type IssueKind string

const (
	IssueFailing        IssueKind = "failing"
	IssueUnreplicated   IssueKind = "unreplicated"
	IssueStaleBackup    IssueKind = "stale-backup"
	IssueStaleSnapshots IssueKind = "stale-snapshots"
	IssueCycleFailing   IssueKind = "cycle-failing"
	IssueSnitchOverdue  IssueKind = "snitch-overdue"
	IssuePaused         IssueKind = "paused"
)

// Issue is one attention-worthy condition, stated so the overview can
// lead with "what needs me": what is wrong, where, since when, and the
// underlying detail (usually an error).
type Issue struct {
	Kind     IssueKind
	Severity Severity
	// Dataset is the affected dataset (or subtree root), nil for
	// system-level issues.
	Dataset *model.DatasetName
	Summary string
	Detail  string
	// Since is when the condition began, zero when unknown.
	Since time.Time
}

// Fulfillment compares one periodicity's retention policy with what is
// actually on hand, per side. Shortfalls are display-only: a young
// dataset legitimately holds fewer snapshots than policy wants.
type Fulfillment struct {
	Periodicity string
	LocalWant   int
	LocalHave   int
	RemoteWant  int
	RemoteHave  int
}

// Dataset is everything the dashboard says about one dataset.
type Dataset struct {
	Name   model.DatasetName
	Health Health
	// Reason is a one-line explanation of a non-ok health.
	Reason string

	// Activity: is this dataset's plan executing right now?
	Syncing bool
	Step    int
	Steps   int

	// Assurance, derived from the inventory and the clock (valid after
	// a restart, unlike run history). HasLocal/HasRemote report whether
	// each side holds any snapshots at all; the ages are meaningful
	// only when the corresponding side does.
	HasLocal     bool
	HasRemote    bool
	NewestLocal  time.Time
	NewestRemote time.Time
	OldestRemote time.Time
	// Snapshotted is the age of the newest local snapshot: are
	// snapshots being taken?
	Snapshotted time.Duration
	// BackedUp is the age of the newest remote snapshot — the recovery
	// point: how much would be lost if the local pool died now?
	BackedUp time.Duration
	// Depth is the age of the oldest remote snapshot: how far back
	// recovery reaches.
	Depth time.Duration
	// Lag is how far the remote trails the local side.
	Lag time.Duration
	// Cadence is the smallest periodicity in the local policy — how
	// often new snapshots are expected to appear. BackupCadence is the
	// smallest periodicity in the remote policy — how fresh the remote
	// can be expected to be: if local snapshots hourly but the remote
	// only retains dailies, a day-old remote is configured behavior,
	// not staleness. Zero means no expectation (no policy).
	Cadence        time.Duration
	BackupCadence  time.Duration
	SnapshotsStale bool
	BackupStale    bool
	Unreplicated   bool

	Fulfillment []Fulfillment

	// Work, from the current plan.
	PendingDeletions int
	PendingTransfers int
	StepsDone        int
	StepsTotal       int

	// Run record, from history (in-memory; empty after a restart).
	LastSuccess time.Time
	LastFailure *history.Failure

	// Configuration.
	Paused        bool // effective: global or subtree
	GlobalPaused  bool
	SubtreePaused bool
	Retention     string
	Overridden    bool

	// Cost.
	LocalCount  int
	RemoteCount int
	LocalUsed   int64
	RemoteUsed  int64
}

// PendingSummary describes queued work, e.g. "2 deletions · 1 transfer".
func (d Dataset) PendingSummary() string {
	var parts []string
	if d.PendingDeletions > 0 {
		parts = append(parts, plural(d.PendingDeletions, "deletion"))
	}
	if d.PendingTransfers > 0 {
		parts = append(parts, plural(d.PendingTransfers, "transfer"))
	}
	return strings.Join(parts, " · ")
}

// Verdict is the one-word answer to "is everything ok?".
type Verdict int

const (
	SystemUnknown Verdict = iota
	SystemOK
	// SystemAttention: something is degraded (warnings) but data is
	// not known to be at risk.
	SystemAttention
	// SystemFailing: at least one critical issue — data at risk or not
	// being backed up.
	SystemFailing
	// SystemPaused: the operator paused execution globally. Wins over
	// everything: it was chosen, and the snitch ping is withheld.
	SystemPaused
)

func (v Verdict) String() string {
	switch v {
	case SystemOK:
		return "ok"
	case SystemAttention:
		return "attention"
	case SystemFailing:
		return "failing"
	case SystemPaused:
		return "paused"
	default:
		return "unknown"
	}
}

// Class is the CSS class suffix for the system verdict chip.
func (v Verdict) Class() string {
	switch v {
	case SystemOK:
		return "ok"
	case SystemAttention:
		return "pending"
	case SystemFailing:
		return "failing"
	case SystemPaused:
		return "paused"
	default:
		return "unknown"
	}
}

// CycleProgress summarizes where the current (or, between cycles, the
// previous) cycle stands: which datasets are done, active, waiting.
type CycleProgress struct {
	Entries   []status.QueueEntry
	Total     int
	Done      int
	Failed    int
	Skipped   int
	Active    model.DatasetName
	HasActive bool
	// Position is the 1-based index of the dataset being worked on:
	// finished + skipped + the active one. "Position of Total".
	Position int
}

// System is the complete derived dashboard state.
type System struct {
	Verdict Verdict
	// Reason explains a non-ok verdict in one line.
	Reason string
	// Issues lists every attention-worthy condition, most severe
	// first. Empty means all clear.
	Issues   []Issue
	Datasets []Dataset
	Cycle    CycleProgress

	DatasetCount int
	LocalUsed    int64
	RemoteUsed   int64

	LastCycle        *history.Cycle
	LastSnitch       time.Time
	SnitchConfigured bool
	SnitchOverdue    bool
}

// Get returns the named dataset's view.
func (s System) Get(name model.DatasetName) (Dataset, bool) {
	for _, ds := range s.Datasets {
		if ds.Name == name {
			return ds, true
		}
	}
	return Dataset{}, false
}

// Input is everything Compute derives from. Now and Boot are explicit
// so the derivations are pure.
type Input struct {
	State    *model.Model
	Conf     *config.Config
	History  *history.History
	Activity status.Activity
	Now      time.Time
	Boot     time.Time
}

// periodicities maps snapshot-name prefixes to their nominal periods,
// in display order.
var periodicities = []struct {
	name   string
	period time.Duration
}{
	{"hourly", time.Hour},
	{"daily", 24 * time.Hour},
	{"weekly", 7 * 24 * time.Hour},
	{"monthly", 30 * 24 * time.Hour},
	{"yearly", 365 * 24 * time.Hour},
}

// Cadence returns the smallest periodicity a policy retains — the
// expected interval between new snapshots — or zero if the policy
// keeps nothing it can name.
func Cadence(policy map[string]int) time.Duration {
	var out time.Duration
	for _, p := range periodicities {
		if policy[p.name] > 0 && (out == 0 || p.period < out) {
			out = p.period
		}
	}
	return out
}

// Compute derives the full dashboard state.
func Compute(in Input) System {
	sys := System{
		LastSnitch:       zeroOr(in.History.LastSnitch()),
		SnitchConfigured: in.Conf.SnitchID != "",
	}
	if cycles := in.History.Cycles(); len(cycles) > 0 {
		sys.LastCycle = &cycles[0]
	}

	var issues []Issue
	if in.State != nil {
		for _, name := range in.State.ListDatasets() {
			ds := computeDataset(in, name, in.State.GetDataset(name))
			sys.Datasets = append(sys.Datasets, ds)
			issues = append(issues, datasetIssues(ds)...)
			sys.LocalUsed += ds.LocalUsed
			sys.RemoteUsed += ds.RemoteUsed
		}
	}
	sys.DatasetCount = len(sys.Datasets)
	issues = append(issues, systemIssues(in, &sys)...)
	sortIssues(issues)
	sys.Issues = issues
	sys.Cycle = cycleProgress(in.Activity)
	sys.Verdict, sys.Reason = verdict(in.Conf, issues)
	return sys
}

func zeroOr(t time.Time, ok bool) time.Time {
	if !ok {
		return time.Time{}
	}
	return t
}

func computeDataset(in Input, name model.DatasetName, m *model.Dataset) Dataset {
	conf := in.Conf
	ds := Dataset{
		Name:          name,
		GlobalPaused:  conf.Paused,
		SubtreePaused: conf.SubtreePaused(name.Path()),
		Paused:        conf.PausedFor(name.Path()),
		Retention:     conf.RetentionDescription(name.Path()),
		LastSuccess:   zeroOr(in.History.LastSuccess(name)),
	}
	if f, ok := in.History.LastFailure(name); ok {
		ds.LastFailure = &f
	}
	_, override := conf.OverrideFor(name.Path())
	ds.Overridden = override != nil

	localPolicy, remotePolicy, _ := conf.PolicyFor(name.Path())
	ds.Cadence = Cadence(localPolicy)
	ds.BackupCadence = Cadence(remotePolicy)

	if m == nil {
		return ds
	}

	var local, remote *model.Snapshots
	if m.Current != nil {
		local, remote = m.Current.Local, m.Current.Remote
	}
	if local != nil && local.Len() > 0 {
		ds.HasLocal = true
		ds.LocalCount = local.Len()
		ds.NewestLocal = local.Newest().Time()
		ds.Snapshotted = in.Now.Sub(ds.NewestLocal)
	}
	if remote != nil && remote.Len() > 0 {
		ds.HasRemote = true
		ds.RemoteCount = remote.Len()
		ds.NewestRemote = remote.Newest().Time()
		ds.OldestRemote = remote.Oldest().Time()
		ds.BackedUp = in.Now.Sub(ds.NewestRemote)
		ds.Depth = in.Now.Sub(ds.OldestRemote)
	}
	if ds.HasLocal && ds.HasRemote {
		ds.Lag = ds.NewestLocal.Sub(ds.NewestRemote)
	}
	if m.Metrics.HasLocal {
		ds.LocalUsed = m.Metrics.LocalSize.Used
	}
	if m.Metrics.HasRemote {
		ds.RemoteUsed = m.Metrics.RemoteSize.Used
	}

	ds.Fulfillment = fulfillment(localPolicy, remotePolicy, local, remote)

	if m.Plan != nil {
		for _, step := range m.Plan.Steps {
			switch step.Status {
			case model.StepCompleted:
				ds.StepsDone++
			case model.StepPending, model.StepInProgress, model.StepFailed:
				if model.IsTransfer(step.Operation) {
					ds.PendingTransfers++
				} else {
					ds.PendingDeletions++
				}
			}
		}
		ds.StepsTotal = len(m.Plan.Steps)
	}

	// Staleness is judged against the relevant cadence with a grace
	// factor: snapshots against the local policy's cadence, the backup
	// against the remote policy's. Pause exempts the backup (transfers
	// were stopped on purpose) but not snapshotting (pause doesn't
	// stop snapshot creation).
	if ds.Cadence > 0 {
		ds.SnapshotsStale = !ds.HasLocal || ds.Snapshotted > StaleFactor*ds.Cadence
	}
	if ds.BackupCadence > 0 {
		ds.BackupStale = !ds.Paused && ds.HasRemote && ds.BackedUp > StaleFactor*ds.BackupCadence
	}
	ds.Unreplicated = ds.HasLocal && !ds.HasRemote

	if a := in.Activity; a.Phase == status.Syncing && a.Dataset == name {
		ds.Syncing = true
		ds.Step, ds.Steps = a.Step, a.Steps
	}

	failing := ds.LastFailure != nil &&
		(ds.LastSuccess.IsZero() || ds.LastFailure.At.After(ds.LastSuccess))

	switch {
	case failing:
		ds.Health = HealthFailing
		ds.Reason = ds.LastFailure.Error
	case ds.Unreplicated:
		ds.Health = HealthAtRisk
		ds.Reason = "never replicated"
	case ds.BackupStale:
		ds.Health = HealthAtRisk
		ds.Reason = fmt.Sprintf("backup %s old", CompactDuration(ds.BackedUp))
	case ds.SnapshotsStale:
		ds.Health = HealthAtRisk
		if ds.HasLocal {
			ds.Reason = fmt.Sprintf("no new snapshots for %s", CompactDuration(ds.Snapshotted))
		} else {
			ds.Reason = "no snapshots"
		}
	case ds.Paused:
		ds.Health = HealthPaused
		if ds.GlobalPaused {
			ds.Reason = "paused globally"
		} else {
			ds.Reason = "paused"
		}
	case ds.PendingDeletions+ds.PendingTransfers > 0:
		ds.Health = HealthBehind
		ds.Reason = ds.PendingSummary() + " queued"
	default:
		ds.Health = HealthOK
	}
	return ds
}

// fulfillment builds the per-periodicity policy-vs-on-hand table. Rows
// appear for every periodicity a policy wants or a side holds, known
// periodicities first in period order, unknown prefixes after,
// alphabetically.
func fulfillment(localPolicy, remotePolicy map[string]int, local, remote *model.Snapshots) []Fulfillment {
	counts := func(snaps *model.Snapshots) map[string]int {
		out := map[string]int{}
		if snaps == nil {
			return out
		}
		for snap := range snaps.All() {
			out[snap.Type()]++
		}
		return out
	}
	localHave, remoteHave := counts(local), counts(remote)

	known := map[string]bool{}
	var names []string
	for _, p := range periodicities {
		known[p.name] = true
		names = append(names, p.name)
	}
	var unknown []string
	for _, m := range []map[string]int{localPolicy, remotePolicy, localHave, remoteHave} {
		for name := range m {
			if !known[name] {
				known[name] = true
				unknown = append(unknown, name)
			}
		}
	}
	sort.Strings(unknown)
	names = append(names, unknown...)

	var out []Fulfillment
	for _, name := range names {
		row := Fulfillment{
			Periodicity: name,
			LocalWant:   localPolicy[name],
			LocalHave:   localHave[name],
			RemoteWant:  remotePolicy[name],
			RemoteHave:  remoteHave[name],
		}
		if row.LocalWant+row.LocalHave+row.RemoteWant+row.RemoteHave > 0 {
			out = append(out, row)
		}
	}
	return out
}

// datasetIssues turns one dataset's alarming conditions into issues.
// A dataset can raise several at once (failing and stale, say): they
// are distinct facts.
func datasetIssues(ds Dataset) []Issue {
	name := ds.Name
	var out []Issue
	if ds.LastFailure != nil && (ds.LastSuccess.IsZero() || ds.LastFailure.At.After(ds.LastSuccess)) {
		out = append(out, Issue{
			Kind:     IssueFailing,
			Severity: Critical,
			Dataset:  &name,
			Summary:  "sync failing",
			Detail:   ds.LastFailure.Error,
			Since:    ds.LastFailure.At,
		})
	}
	if ds.Unreplicated {
		out = append(out, Issue{
			Kind:     IssueUnreplicated,
			Severity: Critical,
			Dataset:  &name,
			Summary:  "never replicated",
			Detail:   "no remote snapshots exist; nothing would survive losing the local pool",
		})
	}
	if ds.BackupStale {
		out = append(out, Issue{
			Kind:     IssueStaleBackup,
			Severity: Critical,
			Dataset:  &name,
			Summary:  fmt.Sprintf("backup %s old", CompactDuration(ds.BackedUp)),
			Detail: fmt.Sprintf("newest remote snapshot is %s old; expected within %s (%d× the %s remote cadence)",
				CompactDuration(ds.BackedUp), CompactDuration(StaleFactor*ds.BackupCadence), StaleFactor, CompactDuration(ds.BackupCadence)),
			Since: ds.NewestRemote.Add(StaleFactor * ds.BackupCadence),
		})
	}
	if ds.SnapshotsStale {
		issue := Issue{
			Kind:     IssueStaleSnapshots,
			Severity: Warning,
			Dataset:  &name,
			Summary:  "snapshots not being taken",
		}
		if ds.HasLocal {
			issue.Detail = fmt.Sprintf("newest local snapshot is %s old; expected within %s (%d× the %s cadence)",
				CompactDuration(ds.Snapshotted), CompactDuration(StaleFactor*ds.Cadence), StaleFactor, CompactDuration(ds.Cadence))
			issue.Since = ds.NewestLocal.Add(StaleFactor * ds.Cadence)
		} else {
			issue.Detail = "no local snapshots exist"
		}
		out = append(out, issue)
	}
	return out
}

// systemIssues covers conditions that belong to the daemon rather than
// a dataset: pause reminders, cycle failures, the snitch.
func systemIssues(in Input, sys *System) []Issue {
	var out []Issue
	conf := in.Conf

	if conf.Paused {
		out = append(out, Issue{
			Kind:     IssuePaused,
			Severity: Info,
			Summary:  "execution paused globally",
			Detail:   "state keeps refreshing but nothing executes, and the snitch ping is withheld — the dead man's switch will eventually fire",
		})
	}
	var pausedKeys []string
	for key, override := range conf.Overrides {
		if override.Paused {
			pausedKeys = append(pausedKeys, normalizeKey(key))
		}
	}
	sort.Strings(pausedKeys)
	for _, key := range pausedKeys {
		name := model.DatasetName(key)
		out = append(out, Issue{
			Kind:     IssuePaused,
			Severity: Info,
			Dataset:  &name,
			Summary:  "subtree paused",
			Detail:   "nothing under this dataset executes until it is resumed",
		})
	}

	// The cycle: currently backing off, or the most recent one failed.
	if a := in.Activity; a.Phase == status.BackingOff {
		issue := Issue{
			Kind:     IssueCycleFailing,
			Severity: Critical,
			Summary:  fmt.Sprintf("%s failed; retrying", plural(a.ConsecutiveFailures, "cycle")),
		}
		if sys.LastCycle != nil {
			issue.Detail = cycleDetail(*sys.LastCycle)
			issue.Since = sys.LastCycle.StoppedAt
		}
		out = append(out, issue)
	} else if c := sys.LastCycle; c != nil && !c.OK {
		out = append(out, Issue{
			Kind:     IssueCycleFailing,
			Severity: Critical,
			Summary:  "last cycle failed",
			Detail:   cycleDetail(*c),
			Since:    c.StoppedAt,
		})
	}

	// The snitch: if configured and not paused, a missing ping means
	// the external alerting chain is about to fire (or believe us dead).
	if sys.SnitchConfigured && !conf.Paused {
		ref := sys.LastSnitch
		if ref.IsZero() {
			ref = in.Boot
		}
		if age := in.Now.Sub(ref); !ref.IsZero() && age > SnitchOverdueFactor*conf.Interval() {
			sys.SnitchOverdue = true
			issue := Issue{
				Kind:     IssueSnitchOverdue,
				Severity: Warning,
				Detail: fmt.Sprintf("expected a successful ping every %s; the dead man's switch will fire",
					CompactDuration(conf.Interval())),
				Since: ref,
			}
			if sys.LastSnitch.IsZero() {
				issue.Summary = "snitch never pinged since boot"
			} else {
				issue.Summary = fmt.Sprintf("snitch not pinged for %s", CompactDuration(age))
			}
			out = append(out, issue)
		}
	}
	return out
}

func cycleDetail(c history.Cycle) string {
	if c.Error != "" {
		return c.Error
	}
	if len(c.Failures) > 0 {
		return "failed datasets: " + strings.Join(c.Failures, ", ")
	}
	return ""
}

// sortIssues orders most severe first; ties break by kind then dataset
// so the order is deterministic.
func sortIssues(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return datasetKey(a.Dataset) < datasetKey(b.Dataset)
	})
}

func datasetKey(name *model.DatasetName) string {
	if name == nil {
		return ""
	}
	return name.Path()
}

// verdict folds the issue list into the one-word system answer.
func verdict(conf *config.Config, issues []Issue) (Verdict, string) {
	if conf.Paused {
		return SystemPaused, "execution is paused; the snitch ping is withheld"
	}
	// Issues arrive sorted most severe first, so the first alarming one
	// is both the worst and the headline.
	alarming := 0
	var top *Issue
	for i := range issues {
		if issues[i].Severity == Info {
			continue
		}
		alarming++
		if top == nil {
			top = &issues[i]
		}
	}
	if top == nil {
		return SystemOK, ""
	}
	worst := top.Severity
	reason := top.Summary
	if top.Dataset != nil {
		reason = top.Dataset.String() + ": " + reason
	}
	if alarming > 1 {
		reason += fmt.Sprintf(" (+%d more)", alarming-1)
	}
	if worst == Critical {
		return SystemFailing, reason
	}
	return SystemAttention, reason
}

func cycleProgress(a status.Activity) CycleProgress {
	c := CycleProgress{Entries: a.Queue, Total: len(a.Queue)}
	for _, e := range a.Queue {
		switch e.State {
		case status.QueueDone:
			c.Done++
		case status.QueueFailed:
			c.Failed++
		case status.QueueSkipped:
			c.Skipped++
		case status.QueueActive:
			c.Active = e.Dataset
			c.HasActive = true
		}
	}
	c.Position = c.Done + c.Failed + c.Skipped
	if c.HasActive {
		c.Position++
	}
	return c
}

func normalizeKey(key string) string {
	key = strings.TrimSuffix(key, "/")
	if !strings.HasPrefix(key, "/") {
		key = "/" + key
	}
	return key
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// CompactDuration renders a duration in its two most significant
// units: "<1m", "42m", "3h12m", "2d4h".
func CompactDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}
