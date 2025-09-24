package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
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

type DatasetSize struct {
	Used              int64  // Total on-disk space with children, including all snapshots
	LogicalReferenced int64  // Logical size of most recent snapshot (w/o children)
}

func (ds *DatasetSize) String() string {
	if ds == nil {
		return "<no size>"
	}
	return humanize.Bytes(uint64(ds.Used))
}

func (ds *DatasetSize) Clone() *DatasetSize {
	if ds == nil {
		return nil
	}
	return &DatasetSize{
		Used:              ds.Used,
		LogicalReferenced: ds.LogicalReferenced,
	}
}

type Dataset struct {
	Name          DatasetName
	Local, Remote *Snapshots
	LocalSize, RemoteSize *DatasetSize
	GoalState     *Dataset // The desired state based on policy
}

func (dataset *Dataset) Staleness() time.Duration {
	local, remote := dataset.Local.Newest(), dataset.Remote.Newest()
	if local == nil || remote == nil {
		return 0
	}
	return local.Time().Sub(remote.Time())
}

func (dataset *Dataset) String() string {
	localSize := ""
	remoteSize := ""
	if dataset.LocalSize != nil {
		localSize = fmt.Sprintf(" %s", humanize.Bytes(uint64(dataset.LocalSize.Used)))
	}
	if dataset.RemoteSize != nil {
		remoteSize = fmt.Sprintf(" %s", humanize.Bytes(uint64(dataset.RemoteSize.Used)))
	}
	return fmt.Sprintf("<%s: %dL%s, %dR%s>", dataset.Name, dataset.Local.Len(), localSize, dataset.Remote.Len(), remoteSize)
}

func (dataset *Dataset) Diff(other *Dataset) string {
	if dataset.Eq(other) {
		return "<no diff>"
	}
	if dataset == nil {
		return "from nil to non-nil"
	}
	if other == nil {
		return "from non-nill to nil"
	}

	var out strings.Builder
	if dataset.Name != other.Name {
		fmt.Fprintf(&out, "  name change from '%s' to '%s'\n", dataset.Name, other.Name)
	}
	fmt.Fprintln(&out, "  local diff")
	fmt.Fprint(&out, dataset.Local.Diff("    ", other.Local))
	fmt.Fprintln(&out, "  remote diff")
	fmt.Fprint(&out, dataset.Remote.Diff("    ", other.Remote))
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
	if !dataset.Local.Eq(other.Local) {
		return false
	}
	if !dataset.Remote.Eq(other.Remote) {
		return false
	}
	return true
}

func (dataset *Dataset) Clone() *Dataset {
	var goalState *Dataset
	if dataset.GoalState != nil {
		goalState = dataset.GoalState.Clone()
	}
	return &Dataset{
		Name:       dataset.Name,
		Local:      dataset.Local.Clone(),
		Remote:     dataset.Remote.Clone(),
		LocalSize:  dataset.LocalSize.Clone(),
		RemoteSize: dataset.RemoteSize.Clone(),
		GoalState:  goalState,
	}
}
