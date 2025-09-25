package model

import (
	"fmt"
	"strings"
	"time"
)

type DatasetName string

const GlobalDataset DatasetName = "global"

func (dn DatasetName) String() string {
	switch dn {
	case "":
		return "<root>"
	default:
		return string(dn)
	}
}

func (dn DatasetName) Path() string {
	return string(dn)
}

//go:generate go run golang.org/x/tools/cmd/stringer -type Location
type Location int

const (
	locationInvalid Location = iota
	Local
	Remote
)

type Dataset struct {
	Name    DatasetName
	Current *SnapshotInventory // Current snapshot state
	Target  *SnapshotInventory // Target snapshot state (from policy)
	Metrics StorageMetrics     // Physical storage metrics
	Plan    Plan               // Plan to get from Current to Target
}

func (dataset *Dataset) Staleness() time.Duration {
	if dataset.Current == nil {
		return 0
	}
	local, remote := dataset.Current.Local.Newest(), dataset.Current.Remote.Newest()
	if local == nil || remote == nil {
		return 0
	}
	return local.Time().Sub(remote.Time())
}

func (dataset *Dataset) String() string {
	if dataset.Current == nil {
		return fmt.Sprintf("<%s: uninitialized>", dataset.Name)
	}

	localCount := 0
	remoteCount := 0
	if dataset.Current.Local != nil {
		localCount = dataset.Current.Local.Len()
	}
	if dataset.Current.Remote != nil {
		remoteCount = dataset.Current.Remote.Len()
	}

	localSize := ""
	remoteSize := ""
	if dataset.Metrics.HasLocal {
		localSize = fmt.Sprintf(" %s", dataset.Metrics.LocalSize.String())
	}
	if dataset.Metrics.HasRemote {
		remoteSize = fmt.Sprintf(" %s", dataset.Metrics.RemoteSize.String())
	}
	return fmt.Sprintf("<%s: %dL%s, %dR%s>", dataset.Name, localCount, localSize, remoteCount, remoteSize)
}

func (dataset *Dataset) Diff(other *Dataset) string {
	if dataset.Eq(other) {
		return "<no diff>"
	}
	if dataset == nil {
		return "from nil to non-nil"
	}
	if other == nil {
		return "from non-nil to nil"
	}

	var out strings.Builder
	if dataset.Name != other.Name {
		fmt.Fprintf(&out, "  name change from '%s' to '%s'\n", dataset.Name, other.Name)
	}

	if dataset.Current != nil && other.Current != nil {
		fmt.Fprintln(&out, "  local diff")
		fmt.Fprint(&out, dataset.Current.Local.Diff("    ", other.Current.Local))
		fmt.Fprintln(&out, "  remote diff")
		fmt.Fprint(&out, dataset.Current.Remote.Diff("    ", other.Current.Remote))
	} else if dataset.Current != nil {
		fmt.Fprintln(&out, "  current inventory removed")
	} else if other.Current != nil {
		fmt.Fprintln(&out, "  current inventory added")
	}

	return out.String()
}

func (dataset *Dataset) Eq(other *Dataset) bool {
	if dataset == nil && other == nil {
		return true
	}
	if dataset == nil || other == nil {
		return false
	}
	if dataset.Name != other.Name {
		return false
	}
	if !dataset.Current.Eq(other.Current) {
		return false
	}
	return true
}

func (dataset *Dataset) Clone() *Dataset {
	// Copy plan (plan steps need to be cloned)
	var plan Plan
	if dataset.Plan != nil {
		plan = make(Plan, len(dataset.Plan))
		for i, step := range dataset.Plan {
			plan[i] = &PlanStep{
				Operation: step.Operation,
				Status:    step.Status,
			}
		}
	}
	return &Dataset{
		Name:    dataset.Name,
		Current: dataset.Current.Clone(),
		Target:  dataset.Target.Clone(),
		Metrics: dataset.Metrics, // Value type, no need to clone
		Plan:    plan,
	}
}
