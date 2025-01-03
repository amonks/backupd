package model

import "fmt"

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
