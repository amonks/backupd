package model

import "fmt"

//go:generate go run golang.org/x/tools/cmd/stringer -type Location
type Location int

const (
	locationInvalid Location = iota
	Local
	Remote
)

type Dataset struct {
	Name          string
	Local, Remote *Snapshots
}

func (dataset *Dataset) String() string {
	return fmt.Sprintf("<%s: %dL, %dR>", dataset.Name, dataset.Local.Len(), dataset.Remote.Len())
}

func (dataset *Dataset) Clone() *Dataset {
	return &Dataset{
		Name:   dataset.Name,
		Local:  dataset.Local.Clone(),
		Remote: dataset.Remote.Clone(),
	}
}
