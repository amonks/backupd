package main

import "strings"

func (snap *Snapshot) String() string {
	return snap.Dataset + "@" + snap.Name
}

func (snap *Snapshot) Type() string {
	return strings.SplitN(snap.Name, "-", 2)[0]
}

func (snap *Snapshot) Title() string {
	return strings.SplitN(snap.Name, "-", 2)[1]
}
