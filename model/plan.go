package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"monks.co/backupd/logger"
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
	Status    StepStatus
	StartedAt *time.Time
	StoppedAt *time.Time
	Logs      *logger.Logger
}

// Duration returns the duration of the step execution
func (ps *PlanStep) Duration() time.Duration {
	if ps.StartedAt == nil || ps.StoppedAt == nil {
		return 0
	}
	return ps.StoppedAt.Sub(*ps.StartedAt)
}

// Verify PlanStep implements Operation
var _ Operation = &PlanStep{}

// String delegates to the wrapped operation
func (ps *PlanStep) String() string {
	return ps.Operation.String()
}

// Apply delegates to the wrapped operation
func (ps *PlanStep) Apply(inv *SnapshotInventory) (*SnapshotInventory, error) {
	return ps.Operation.Apply(inv)
}

// Plan is a sequence of plan steps with plan-level logging
type Plan struct {
	Steps []*PlanStep
}

// NewPlanStep creates a new plan step with pending status
func NewPlanStep(op Operation) *PlanStep {
	return &PlanStep{
		Operation: op,
		Status:    StepPending,
		Logs:      logger.New(op.String()),
	}
}

// TryExecute runs work as the step, recording its status and timing on
// ps and handing each change to publish as well, to apply to the copy
// of the step that others read. ps is the caller's own: a step that
// another goroutine may hold is never written, only replaced, so
// publish applies the change to a clone it then publishes. Each change
// is computed once, so both copies say the same times.
func (ps *PlanStep) TryExecute(publish func(func(*PlanStep)), work func() error) error {
	started := time.Now()
	start := func(s *PlanStep) {
		s.StartedAt = &started
		s.Status = StepInProgress
	}
	start(ps)
	publish(start)

	err := work()

	stopped := time.Now()
	status := StepCompleted
	if err != nil {
		status = StepFailed
	}
	stop := func(s *PlanStep) {
		s.StoppedAt = &stopped
		s.Status = status
	}
	stop(ps)
	publish(stop)

	return err
}

// Clone returns a copy of the plan whose steps can be updated without
// touching p's. The operations and logs are shared: neither is written
// through a step.
func (p *Plan) Clone() *Plan {
	if p == nil {
		return nil
	}
	steps := make([]*PlanStep, len(p.Steps))
	for i, step := range p.Steps {
		clone := *step
		steps[i] = &clone
	}
	return &Plan{Steps: steps}
}

// PlanFromOperations converts a slice of operations to a Plan
func PlanFromOperations(ops []Operation) *Plan {
	steps := make([]*PlanStep, len(ops))
	for i, op := range ops {
		steps[i] = NewPlanStep(op)
	}
	return &Plan{
		Steps: steps,
	}
}

func CalculateTransitionPlan(current, target *SnapshotInventory) (*Plan, error) {
	var ops []Operation

	localDeletions := current.Local.Difference(target.Local)
	remoteDeletions := current.Remote.Difference(target.Remote)

	localDeletionRanges := current.Local.GroupByAdjacency(localDeletions)
	remoteDeletionRanges := current.Remote.GroupByAdjacency(remoteDeletions)

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

	transfers := target.Remote.Difference(current.Remote)
	if transfers.Len() == 0 {
		return PlanFromOperations(ops), nil
	}

	sharedSnapshots := current.Remote.Intersection(current.Local)

	// if there is no shared snapshot, but there are remote snapshots, error
	last := sharedSnapshots.Newest()
	if last == nil && current.Remote.Len() > 0 {
		return nil, fmt.Errorf("remote has data, but none is shared with local")
	}
	if current.Remote.Len() == 0 {
		ops = append(ops, &InitialSnapshotTransfer{
			Snapshot: transfers.Oldest(),
		})
		last = transfers.Oldest()
		transfers.Del(transfers.Oldest())
	}
	if last == nil || !current.Local.Has(last) {
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

func ValidatePlan(ctx context.Context, current, target *SnapshotInventory, plan *Plan, isDebugging bool) error {
	if isDebugging {
		fmt.Println("PLAN STEPS")
	}

	out := current.Clone()
	for _, op := range plan.Steps {
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
	if !target.Eq(out) {
		errors = append(errors, fmt.Sprintf("flaws are:\n%s", target.Diff(out)))
	}

	if errors != nil {
		return fmt.Errorf("applying plan does not produce target state:\n%s", strings.Join(errors, "\n"))
	}
	return nil
}
