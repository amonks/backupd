package model

import (
	"fmt"
	"strings"
)

// SnapshotInventory represents the pure logical state of what snapshots exist where.
// It contains no physical storage metrics, only the snapshot collections themselves.
// This is used for planning operations and determining goal states.
type SnapshotInventory struct {
	Local  *Snapshots
	Remote *Snapshots
}

// NewSnapshotInventory creates a new SnapshotInventory with the given local and remote snapshots
func NewSnapshotInventory(local, remote *Snapshots) *SnapshotInventory {
	return &SnapshotInventory{
		Local:  local,
		Remote: remote,
	}
}

// Clone creates a deep copy of the SnapshotInventory
func (si *SnapshotInventory) Clone() *SnapshotInventory {
	if si == nil {
		return nil
	}
	return &SnapshotInventory{
		Local:  si.Local.Clone(),
		Remote: si.Remote.Clone(),
	}
}

// Eq checks if two SnapshotInventories are equal
func (si *SnapshotInventory) Eq(other *SnapshotInventory) bool {
	if si == nil && other == nil {
		return true
	}
	if si == nil || other == nil {
		return false
	}
	if !si.Local.Eq(other.Local) {
		return false
	}
	if !si.Remote.Eq(other.Remote) {
		return false
	}
	return true
}

// Diff returns a string describing the differences between two SnapshotInventories
func (si *SnapshotInventory) Diff(other *SnapshotInventory) string {
	if si.Eq(other) {
		return "<no diff>"
	}
	if si == nil {
		return "from nil to non-nil"
	}
	if other == nil {
		return "from non-nil to nil"
	}

	var out strings.Builder
	fmt.Fprintln(&out, "  local diff")
	fmt.Fprint(&out, si.Local.Diff("    ", other.Local))
	fmt.Fprintln(&out, "  remote diff")
	fmt.Fprint(&out, si.Remote.Diff("    ", other.Remote))
	return out.String()
}

// LocalString returns the string representation of local snapshots or "-" if nil
func (si *SnapshotInventory) LocalString() string {
	if si == nil || si.Local == nil {
		return "-"
	}
	return si.Local.String()
}

// RemoteString returns the string representation of remote snapshots or "-" if nil
func (si *SnapshotInventory) RemoteString() string {
	if si == nil || si.Remote == nil {
		return "-"
	}
	return si.Remote.String()
}
