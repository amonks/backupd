package model

import (
	"context"
	"fmt"
	"strings"
)

func (dataset *Dataset) Plan(goal *Dataset) ([]Operation, error) {
	var plan []Operation

	localDeletions := dataset.Local.Difference(goal.Local)
	remoteDeletions := dataset.Remote.Difference(goal.Remote)

	localDeletionRanges := dataset.Local.GroupByAdjacency(localDeletions)
	remoteDeletionRanges := dataset.Remote.GroupByAdjacency(remoteDeletions)

	for _, del := range localDeletionRanges {
		if del.Len() == 1 {
			plan = append(plan, &SnapshotDeletion{
				Location: Local,
				Snapshot: del.Oldest(),
			})
		} else {
			plan = append(plan, &SnapshotRangeDeletion{
				Location: Local,
				Start:    del.Oldest(),
				End:      del.Newest(),
			})
		}
	}

	for _, del := range remoteDeletionRanges {
		if del.Len() == 1 {
			plan = append(plan, &SnapshotDeletion{
				Location: Remote,
				Snapshot: del.Oldest(),
			})
		} else {
			plan = append(plan, &SnapshotRangeDeletion{
				Location: Remote,
				Start:    del.Oldest(),
				End:      del.Newest(),
			})
		}
	}

	transfers := goal.Remote.Difference(dataset.Remote)
	if transfers.Len() == 0 {
		return plan, nil
	}

	sharedSnapshots := dataset.Remote.Intersection(dataset.Local)

	// if there is no shared snapshot, but there are remote snapshots, error
	last := sharedSnapshots.Newest()
	if last == nil && dataset.Remote.Len() > 0 {
		return nil, fmt.Errorf("remote has data, but none is shared with local")
	}
	if dataset.Remote.Len() == 0 {
		plan = append(plan, &InitialSnapshotTransfer{
			Snapshot: transfers.Oldest(),
		})
		last = transfers.Oldest()
		transfers.Del(transfers.Oldest())
	}
	if last == nil || !dataset.Local.Has(last) {
		return nil, fmt.Errorf("local doesn't have transfer base snapshot %s", last)
	}
	for snapshot := range transfers.All() {
		plan = append(plan, &SnapshotRangeTransfer{
			Start: last,
			End:   snapshot,
		})
		last = snapshot
	}

	return plan, nil
}

func (dataset *Dataset) ValidatePlan(ctx context.Context, goal *Dataset, plan []Operation, isDebugging bool) error {
	debug := func(v string, args ...any) {
		if isDebugging {
			fmt.Printf(v+"\n", args...)
		}
	}

	debug("PLAN STEPS")

	out := dataset.Clone()
	for _, op := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, err := op.Apply(out)

		debug("-- %s", op)
		debug(out.Diff(got))
		debug("")

		out = got
		if err != nil {
			return fmt.Errorf("invalid operation %s: %w", op, err)
		}
	}

	var errors []string
	if !goal.Eq(out) {
		errors = append(errors, fmt.Sprintf("flaws are:\n%s", goal.Diff(out)))
	}

	if errors != nil {
		return fmt.Errorf("applying %s to %s does not produce %s:\n%s", plan, dataset, goal, strings.Join(errors, "\n"))
	}
	return nil
}
