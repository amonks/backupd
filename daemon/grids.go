package daemon

// The dashboard's tables render through pkg/datagrid: the complete data
// set travels with the page (every ring history holds is bounded), and
// the grid adds search, facets, typed sorting, and pagination in the
// browser. Each grid's state lives in the URL query string, so it
// survives the dashboard's body-swapping live refresh.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/a-h/templ"
	"monks.co/backupd/history"
	"monks.co/backupd/model"
	"monks.co/backupd/view"
	"monks.co/pkg/datagrid"
	"monks.co/pkg/localtime"
)

// ageSortValue keys age columns for sorting. A missing side gets the
// empty (malformed) value, which datagrid keeps last in both
// directions — "never" belongs at the end however you sort.
func ageSortValue(has bool, age time.Duration) string {
	if !has {
		return ""
	}
	return fmt.Sprint(int64(age.Seconds()))
}

// healthFilterValue gives every health its own facet entry. The
// stable value is the operator-facing label, not the CSS class, so a
// bookmarked filter URL is not coupled to a stylesheet detail.
func healthFilterValue(h view.Health) []datagrid.FilterValue {
	return []datagrid.FilterValue{{Value: h.String(), Label: h.String()}}
}

// rowIDForDataset keys grid rows by dataset. The root dataset's path is
// the empty string, which is not a valid row ID.
func rowIDForDataset(name model.DatasetName) string {
	if name == "" {
		return "root"
	}
	return name.Path()
}

func fleetGrid(base string, datasets []view.Dataset) templ.Component {
	return datagrid.Table(datagrid.Options[view.Dataset]{
		ID:                "fleet",
		PageSize:          50,
		SearchPlaceholder: "Search datasets…",
		RowID:             func(dv view.Dataset) string { return rowIDForDataset(dv.Name) },
		Columns: []datagrid.Column[view.Dataset]{
			{
				Key: "dataset", Label: "dataset", RowHeader: true,
				Text:     func(dv view.Dataset) string { return dv.Name.String() },
				Cell:     func(dv view.Dataset) templ.Component { return datasetLink(base, dv.Name) },
				FilterUI: datagrid.FilterNone,
			},
			{
				Key: "status", Label: "status",
				Text:         func(dv view.Dataset) string { return dv.Health.String() },
				SearchText:   func(dv view.Dataset) string { return dv.Health.String() + " " + dv.Reason },
				SortKind:     datagrid.SortNumber,
				Align:        "start",
				SortValue:    func(dv view.Dataset) string { return fmt.Sprint(int(dv.Health)) },
				FilterValues: func(dv view.Dataset) []datagrid.FilterValue { return healthFilterValue(dv.Health) },
				FilterUI:     datagrid.FilterMenu,
				Cell:         func(dv view.Dataset) templ.Component { return healthChip(dv) },
			},
			{
				Key: "snapshotted", Label: "snapshotted",
				Header: headerTitle("snapshotted", "Age of the newest local snapshot"),
				Text: func(dv view.Dataset) string {
					if !dv.HasLocal {
						return "no snapshots"
					}
					return fmtCompactDuration(dv.Snapshotted)
				},
				SortKind:  datagrid.SortNumber,
				SortValue: func(dv view.Dataset) string { return ageSortValue(dv.HasLocal, dv.Snapshotted) },
				Cell: func(dv view.Dataset) templ.Component {
					return ageCell(dv.HasLocal, dv.Snapshotted, dv.SnapshotsStale, "no snapshots")
				},
				FilterUI: datagrid.FilterNone,
				Disabled: datagrid.FeatureSearch,
			},
			{
				Key: "backedup", Label: "backed up",
				Header: headerTitle("backed up", "Age of the newest remote snapshot — the recovery point"),
				Text: func(dv view.Dataset) string {
					if !dv.HasRemote {
						return "never"
					}
					return fmtCompactDuration(dv.BackedUp)
				},
				SortKind:  datagrid.SortNumber,
				SortValue: func(dv view.Dataset) string { return ageSortValue(dv.HasRemote, dv.BackedUp) },
				Cell: func(dv view.Dataset) templ.Component {
					return ageCell(dv.HasRemote, dv.BackedUp, dv.BackupStale, "never")
				},
				FilterUI: datagrid.FilterNone,
				Disabled: datagrid.FeatureSearch,
			},
			{
				Key: "pending", Label: "pending",
				Text: func(dv view.Dataset) string {
					if s := dv.PendingSummary(); s != "" {
						return s
					}
					return "—"
				},
				SortKind:  datagrid.SortNumber,
				Align:     "start",
				SortValue: func(dv view.Dataset) string { return fmt.Sprint(dv.PendingDeletions + dv.PendingTransfers) },
				Cell:      func(dv view.Dataset) templ.Component { return pendingCell(dv) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "local", Label: "local",
				Text: func(dv view.Dataset) string {
					return fmt.Sprintf("%d · %s", dv.LocalCount, fmtBytes(dv.LocalUsed))
				},
				SortKind:  datagrid.SortNumber,
				SortValue: func(dv view.Dataset) string { return fmt.Sprint(dv.LocalUsed) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "remote", Label: "remote",
				Text: func(dv view.Dataset) string {
					return fmt.Sprintf("%d · %s", dv.RemoteCount, fmtBytes(dv.RemoteUsed))
				},
				SortKind:  datagrid.SortNumber,
				SortValue: func(dv view.Dataset) string { return fmt.Sprint(dv.RemoteUsed) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "retention", Label: "retention",
				Text: func(dv view.Dataset) string {
					if !dv.Overridden {
						return "default"
					}
					return dv.Retention
				},
				Cell:     func(dv view.Dataset) templ.Component { return retentionCell(dv) },
				FilterUI: datagrid.FilterMenu,
			},
		},
	}, datasets)
}

func opsGrid(base string, ops []history.Op) templ.Component {
	return datagrid.Table(datagrid.Options[history.Op]{
		ID:                "ops",
		SearchPlaceholder: "Search operations…",
		Columns: []datagrid.Column[history.Op]{
			{
				Key: "when", Label: "when",
				Text:      func(op history.Op) string { return fmtAgo(op.At) },
				SortKind:  datagrid.SortNumber,
				Align:     "start",
				SortValue: func(op history.Op) string { return fmt.Sprint(op.At.UnixMilli()) },
				Cell:      func(op history.Op) templ.Component { return whenCell(op.At) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "dataset", Label: "dataset",
				Text: func(op history.Op) string { return op.Dataset.String() },
				Cell: func(op history.Op) templ.Component { return datasetLink(base, op.Dataset) },
			},
			{
				Key: "operation", Label: "operation",
				Text: func(op history.Op) string { return op.Operation },
				FilterValues: func(op history.Op) []datagrid.FilterValue {
					kind := op.Kind
					if kind == "" {
						kind = "other"
					}
					return []datagrid.FilterValue{{Value: kind, Label: kind}}
				},
				FilterUI: datagrid.FilterMenu,
				Cell:     func(op history.Op) templ.Component { return codeCell(op.Operation) },
			},
			{
				Key: "duration", Label: "duration",
				Text:      func(op history.Op) string { return op.Duration.Round(time.Millisecond).String() },
				SortKind:  datagrid.SortNumber,
				SortValue: func(op history.Op) string { return fmt.Sprint(op.Duration.Milliseconds()) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "result", Label: "result",
				Text: func(op history.Op) string {
					if op.Error == "" {
						return "ok"
					}
					return "error"
				},
				SearchText: func(op history.Op) string { return op.Error },
				FilterUI:   datagrid.FilterMenu,
				Cell:       func(op history.Op) templ.Component { return opResultCell(op) },
			},
		},
	}, ops)
}

func journalGrid(base string, events []history.Event) templ.Component {
	return datagrid.Table(datagrid.Options[history.Event]{
		ID:                "journal",
		SearchPlaceholder: "Search the journal…",
		Columns: []datagrid.Column[history.Event]{
			{
				Key: "when", Label: "when",
				Text:      func(e history.Event) string { return fmtAgo(e.At) },
				SortKind:  datagrid.SortNumber,
				Align:     "start",
				SortValue: func(e history.Event) string { return fmt.Sprint(e.At.UnixMilli()) },
				Cell:      func(e history.Event) templ.Component { return whenCell(e.At) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "level", Label: "level",
				Text:     func(e history.Event) string { return string(e.Level) },
				FilterUI: datagrid.FilterMenu,
				Cell:     func(e history.Event) templ.Component { return levelChip(e.Level) },
			},
			{
				Key: "dataset", Label: "dataset",
				Text: func(e history.Event) string {
					if e.Dataset == nil {
						return "—"
					}
					return e.Dataset.String()
				},
				Cell: func(e history.Event) templ.Component {
					if e.Dataset == nil {
						return mutedDash()
					}
					return datasetLink(base, *e.Dataset)
				},
			},
			{
				Key: "message", Label: "message",
				Text:     func(e history.Event) string { return e.Message },
				FilterUI: datagrid.FilterNone,
			},
		},
	}, events)
}

func runsGrid(runs []view.CycleRun) templ.Component {
	return datagrid.Table(datagrid.Options[view.CycleRun]{
		ID:                "cycles",
		SearchPlaceholder: "Search cycle history…",
		Columns: []datagrid.Column[view.CycleRun]{
			{
				Key: "period", Label: "period",
				Text:      fmtRunPeriod,
				Cell:      runPeriodCell,
				SortKind:  datagrid.SortNumber,
				Align:     "start",
				SortValue: func(r view.CycleRun) string { return fmt.Sprint(r.Last.UnixMilli()) },
				FilterUI:  datagrid.FilterNone,
			},
			{
				Key: "result", Label: "result",
				Text:     func(r view.CycleRun) string { return r.Outcome.String() },
				FilterUI: datagrid.FilterMenu,
				Cell:     func(r view.CycleRun) templ.Component { return outcomeCell(r.Outcome) },
			},
			{
				Key: "cycles", Label: "cycles",
				Text:      func(r view.CycleRun) string { return fmt.Sprint(r.Count) },
				SortKind:  datagrid.SortNumber,
				SortValue: func(r view.CycleRun) string { return fmt.Sprint(r.Count) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "avg", Label: "avg duration",
				Text:      func(r view.CycleRun) string { return r.AvgDuration.Round(time.Second).String() },
				SortKind:  datagrid.SortNumber,
				SortValue: func(r view.CycleRun) string { return fmt.Sprint(int64(r.AvgDuration.Seconds())) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			{
				Key: "detail", Label: "detail",
				Text:     func(r view.CycleRun) string { return r.Detail },
				Cell:     func(r view.CycleRun) templ.Component { return codeCell(r.Detail) },
				FilterUI: datagrid.FilterNone,
			},
		},
	}, runs)
}

// fmtRunPeriod names a run's span as the grid's search text — one
// cycle by its start, a run by its oldest start and newest stop — in
// marked UTC; runPeriodCell is the same span as localtime stamps.
func fmtRunPeriod(r view.CycleRun) string {
	if r.Count == 1 {
		return localtime.Fallback(r.First, localtime.Second)
	}
	return localtime.Fallback(r.First, localtime.Second) + " → " + localtime.Fallback(r.Last, localtime.Second)
}

func runPeriodCell(r view.CycleRun) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := localtime.Stamp(r.First, localtime.Second).Render(ctx, w); err != nil {
			return err
		}
		if r.Count == 1 {
			return nil
		}
		if _, err := io.WriteString(w, " → "); err != nil {
			return err
		}
		return localtime.Stamp(r.Last, localtime.Second).Render(ctx, w)
	})
}

// snapPresence describes one side's relationship to a snapshot: what is
// true now, and what the plan will make true.
type snapPresence struct {
	Value string // stable facet value
	Label string
}

var (
	presencePresent = snapPresence{"present", "present"}
	presenceDoomed  = snapPresence{"doomed", "scheduled for deletion"}
	presenceQueued  = snapPresence{"queued", "scheduled for transfer"}
	presenceAbsent  = snapPresence{"absent", "absent"}
)

func presenceOf(current, inTarget, hasTarget bool) snapPresence {
	switch {
	case current && hasTarget && !inTarget:
		return presenceDoomed
	case current:
		return presencePresent
	case inTarget:
		return presenceQueued
	default:
		return presenceAbsent
	}
}

// snapRow is one snapshot's grid row, precomputed from the dataset's
// current and target inventories.
type snapRow struct {
	Snap   *model.Snapshot
	Local  snapPresence
	Remote snapPresence
}

func snapshotRowsFor(ds *model.Dataset) []snapRow {
	if ds.Current == nil {
		return nil
	}
	var out []snapRow
	hasTarget := ds.Target != nil
	for snap := range ds.Current.Local.Union(ds.Current.Remote).AllDesc() {
		out = append(out, snapRow{
			Snap:   snap,
			Local:  presenceOf(ds.Current.Local.Has(snap), targetHas(ds.Target, model.Local, snap), hasTarget),
			Remote: presenceOf(ds.Current.Remote.Has(snap), targetHas(ds.Target, model.Remote, snap), hasTarget),
		})
	}
	return out
}

func snapshotsGrid(ds *model.Dataset) templ.Component {
	presenceColumn := func(key, label string, side func(snapRow) snapPresence) datagrid.Column[snapRow] {
		return datagrid.Column[snapRow]{
			Key: key, Label: label,
			Text: func(r snapRow) string { return side(r).Label },
			FilterValues: func(r snapRow) []datagrid.FilterValue {
				p := side(r)
				return []datagrid.FilterValue{{Value: p.Value, Label: p.Label}}
			},
			FilterUI: datagrid.FilterMenu,
			Align:    "center",
			Cell:     func(r snapRow) templ.Component { return presenceCell(side(r)) },
			Disabled: datagrid.FeatureSearch,
		}
	}
	return datagrid.Table(datagrid.Options[snapRow]{
		ID:                "snapshots",
		PageSize:          50,
		SearchPlaceholder: "Search snapshots…",
		RowID:             func(r snapRow) string { return r.Snap.ID() },
		Columns: []datagrid.Column[snapRow]{
			{
				Key: "snapshot", Label: "snapshot", RowHeader: true,
				Text:     func(r snapRow) string { return r.Snap.Name },
				FilterUI: datagrid.FilterNone,
			},
			{
				Key: "type", Label: "type",
				Text:     func(r snapRow) string { return r.Snap.Type() },
				FilterUI: datagrid.FilterMenu,
				Disabled: datagrid.FeatureSearch,
			},
			{
				Key: "created", Label: "created",
				Text:      func(r snapRow) string { return localtime.Fallback(r.Snap.Time(), localtime.Second) },
				Cell:      func(r snapRow) templ.Component { return localtime.Stamp(r.Snap.Time(), localtime.Second) },
				SortKind:  datagrid.SortNumber,
				Align:     "start",
				SortValue: func(r snapRow) string { return fmt.Sprint(r.Snap.CreatedAt) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
			presenceColumn("local", "local", func(r snapRow) snapPresence { return r.Local }),
			presenceColumn("remote", "remote", func(r snapRow) snapPresence { return r.Remote }),
			{
				Key: "size", Label: "size",
				Text:      func(r snapRow) string { return r.Snap.SizeString() },
				SortKind:  datagrid.SortNumber,
				SortValue: func(r snapRow) string { return fmt.Sprint(r.Snap.LogicalReferenced) },
				Cell:      func(r snapRow) templ.Component { return codeCell(r.Snap.SizeString()) },
				FilterUI:  datagrid.FilterNone,
				Disabled:  datagrid.FeatureSearch,
			},
		},
	}, snapshotRowsFor(ds))
}
