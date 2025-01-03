package model

import "fmt"

type Model struct {
	Datasets map[DatasetName]*Dataset
}

func (model *Model) Clone() *Model {
	datasets := make(map[DatasetName]*Dataset, len(model.Datasets))
	for _, dataset := range model.Datasets {
		datasets[dataset.Name] = dataset.Clone()
	}
	return &Model{datasets}
}

func (model *Model) String() string {
	local, remote := 0, 0
	for _, dataset := range model.Datasets {
		local += dataset.Local.Len()
		remote += dataset.Remote.Len()
	}
	return fmt.Sprintf("<%d datasets, %dL, %dR>", len(model.Datasets), local, remote)
}
