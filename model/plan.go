package model

import (
	"context"
	"fmt"
	"strings"
)

// StepStatus represents the execution status of a plan step
type StepStatus int

const (
	StepPending StepStatus = iota
	StepInProgress
	StepCompleted
	StepFailed
)

// PlanStep wraps an Operation with its execution status
type PlanStep struct {
	Operation
	Status StepStatus
}

// Verify PlanStep implements Operation
var _ Operation = &PlanStep{}

// String delegates to the wrapped operation
func (ps *PlanStep) String() string {
	return ps.Operation.String()
}

// Apply delegates to the wrapped operation
func (ps *PlanStep) Apply(ds *Dataset) (*Dataset, error) {
	return ps.Operation.Apply(ds)
}

// Plan is a sequence of plan steps
type Plan []*PlanStep

// NewPlanStep creates a new plan step with pending status
func NewPlanStep(op Operation) *PlanStep {
	return &PlanStep{
		Operation: op,
		Status:    StepPending,
	}
}

// PlanFromOperations converts a slice of operations to a Plan
func PlanFromOperations(ops []Operation) Plan {
	steps := make(Plan, len(ops))
	for i, op := range ops {
		steps[i] = NewPlanStep(op)
	}
	return steps
}

func (dataset *Dataset) Plan(goal *Dataset) (Plan, error) {
	var ops []Operation

	localDeletions := dataset.Local.Difference(goal.Local)
	remoteDeletions := dataset.Remote.Difference(goal.Remote)

	localDeletionRanges := dataset.Local.GroupByAdjacency(localDeletions)
	remoteDeletionRanges := dataset.Remote.GroupByAdjacency(remoteDeletions)

	for _, del := range localDeletionRanges {
		if del.Len() == 1 {
			ops = append(ops, &SnapshotDeletion{
				Location: Local,
				Snapshot: del.Oldest(),
			})
		} else {
			ops = append(ops, &SnapshotRangeDeletion{
				Location: Local,
				Start:    del.Oldest(),
				End:      del.Newest(),
			})
		}
	}

	for _, del := range remoteDeletionRanges {
		if del.Len() == 1 {
			ops = append(ops, &SnapshotDeletion{
				Location: Remote,
				Snapshot: del.Oldest(),
			})
		} else {
			ops = append(ops, &SnapshotRangeDeletion{
				Location: Remote,
				Start:    del.Oldest(),
				End:      del.Newest(),
			})
		}
	}

	transfers := goal.Remote.Difference(dataset.Remote)
	if transfers.Len() == 0 {
		return PlanFromOperations(ops), nil
	}

	sharedSnapshots := dataset.Remote.Intersection(dataset.Local)

	// if there is no shared snapshot, but there are remote snapshots, error
	last := sharedSnapshots.Newest()
	if last == nil && dataset.Remote.Len() > 0 {
		return nil, fmt.Errorf("remote has data, but none is shared with local")
	}
	if dataset.Remote.Len() == 0 {
		ops = append(ops, &InitialSnapshotTransfer{
			Snapshot: transfers.Oldest(),
		})
		last = transfers.Oldest()
		transfers.Del(transfers.Oldest())
	}
	if last == nil || !dataset.Local.Has(last) {
		return nil, fmt.Errorf("local doesn't have transfer base snapshot %s", last)
	}
	for snapshot := range transfers.All() {
		ops = append(ops, &SnapshotRangeTransfer{
			Start: last,
			End:   snapshot,
		})
		last = snapshot
	}

	return PlanFromOperations(ops), nil
}

func (dataset *Dataset) ValidatePlan(ctx context.Context, goal *Dataset, plan Plan, isDebugging bool) error {
	if isDebugging {
		fmt.Println("PLAN STEPS")
	}

	out := dataset.Clone()
	for _, op := range plan {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, err := op.Apply(out)

		if isDebugging {
			fmt.Printf("-- %s\n", op)
			fmt.Println(out.Diff(got))
			fmt.Println()
		}

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
