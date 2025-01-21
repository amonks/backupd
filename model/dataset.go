package model

import (
	"fmt"
	"strings"
	"time"
)

type DatasetName string

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
	Name          DatasetName
	Local, Remote *Snapshots
}

func (dataset *Dataset) Staleness() time.Duration {
	local, remote := dataset.Local.Newest(), dataset.Remote.Newest()
	if local == nil || remote == nil {
		return 0
	}
	return local.Time().Sub(remote.Time())
}

func (dataset *Dataset) String() string {
	return fmt.Sprintf("<%s: %dL, %dR>", dataset.Name, dataset.Local.Len(), dataset.Remote.Len())
}

func (dataset *Dataset) Diff(other *Dataset) string {
	if dataset.Eq(other) {
		return "<no diff>"
	}

	var out strings.Builder
	if dataset.Name != other.Name {
		fmt.Fprintf(&out, "  name change from '%s' to '%s'\n", dataset.Name, other.Name)
	}
	fmt.Fprintln(&out, "  local diff")
	fmt.Fprintf(&out, dataset.Local.Diff("    ", other.Local))
	fmt.Fprintln(&out, "  remote diff")
	fmt.Fprintf(&out, dataset.Remote.Diff("    ", other.Remote))
	return out.String()
}

func (dataset *Dataset) Eq(other *Dataset) bool {
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
	return &Dataset{
		Name:   dataset.Name,
		Local:  dataset.Local.Clone(),
		Remote: dataset.Remote.Clone(),
	}
}
