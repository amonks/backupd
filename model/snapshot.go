package model

import (
	"fmt"
	"strings"
	"time"
)

type Snapshot struct {
	Dataset   DatasetName
	Name      string
	CreatedAt int64
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
	return snap.Dataset.Path() + "@" + snap.Name
}

func (snap *Snapshot) Type() string {
	return strings.SplitN(snap.Name, "-", 2)[0]
}

func (snap *Snapshot) Title() string {
	return strings.SplitN(snap.Name, "-", 2)[1]
}
