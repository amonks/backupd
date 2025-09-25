package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

type Snapshot struct {
	Dataset           DatasetName
	Name              string
	CreatedAt         int64
	LogicalReferenced int64 // Logical size of dataset at this snapshot (w/o children)
}

func (snap *Snapshot) ID() string {
	return fmt.Sprintf("%s-%s", snap.Dataset, snap.Name)
}

func (snap *Snapshot) Eq(other *Snapshot) bool {
	return snap.ID() == other.ID()
}

func (snap *Snapshot) Time() time.Time {
	return time.Unix(snap.CreatedAt, 0)
}

func (snap *Snapshot) String() string {
	if snap.LogicalReferenced > 0 {
		return fmt.Sprintf("%s@%s (%s)", snap.Dataset.Path(), snap.Name, humanize.Bytes(uint64(snap.LogicalReferenced)))
	}
	return snap.Dataset.Path() + "@" + snap.Name
}

func (snap *Snapshot) SizeString() string {
	if snap.LogicalReferenced > 0 {
		return humanize.Bytes(uint64(snap.LogicalReferenced))
	}
	return "-"
}

func (snap *Snapshot) Type() string {
	return strings.SplitN(snap.Name, "-", 2)[0]
}

func (snap *Snapshot) Title() string {
	return strings.SplitN(snap.Name, "-", 2)[1]
}

func (snap *Snapshot) Less(other *Snapshot) bool {
	if snap.CreatedAt == other.CreatedAt {
		return snap.Name < other.Name
	}
	return snap.CreatedAt < other.CreatedAt
}

func (snap *Snapshot) More(other *Snapshot) bool {
	if snap.CreatedAt == other.CreatedAt {
		return snap.Name > other.Name
	}
	return snap.CreatedAt > other.CreatedAt
}
