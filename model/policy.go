package model

import (
	_ "embed"
	"encoding/json"
)

type Policy struct {
	Local  map[string]int
	Remote map[string]int
}

//go:embed policy.json
var policyJSON []byte

var policy Policy

func init() {
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		panic(err)
	}
}
