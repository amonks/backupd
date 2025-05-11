package progress

import (
	"fmt"
	"iter"
	"sort"
	"time"

	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

type Progress struct {
	*atom.Atom[Value]
}

type Value map[model.DatasetName]*Process

func (v Value) All() iter.Seq2[model.DatasetName, []LogEntry] {
	var ks []model.DatasetName
	for k := range v {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if string(ks[i]) == "global" {
			return true
		} else if string(ks[j]) == "global" {
			return false
		}
		if len(ks[i]) == len(ks[j]) {
			return ks[i] < ks[j]
		}
		return len(ks[i]) < len(ks[j])
	})
	return func(yield func(model.DatasetName, []LogEntry) bool) {
		for _, k := range ks {
			if !yield(k, v[k].logs) {
				return
			}
		}
	}
}

func (v Value) Get(k model.DatasetName) []LogEntry {
	if v[k] == nil {
		return nil
	}
	return v[k].logs
}

type Process struct {
	isDone bool
	logs   []LogEntry
}

type LogEntry struct {
	LogAt time.Time
	Log   string
}

func New() *Progress {
	return &Progress{
		atom.New(Value{}),
	}
}

func (pr *Progress) Deref() Value {
	return pr.Atom.Deref()
}

func (pr *Progress) Log(ds model.DatasetName, s string, args ...any) {
	pr.Swap(func(old Value) Value {
		entry := LogEntry{
			LogAt: time.Now(),
			Log:   fmt.Sprintf(s, args...),
		}

		out := make(Value, len(old))
		seen := false
		for k, v := range old {
			if k != ds {
				out[k] = v
				continue
			}

			seen = true
			if old[k].isDone {
				out[k] = &Process{
					isDone: false,
					logs:   []LogEntry{entry},
				}
			} else {
				out[k] = &Process{
					isDone: false,
					logs:   append(v.logs, entry),
				}
			}
		}
		if !seen {
			out[ds] = &Process{
				logs: []LogEntry{entry},
			}
		}
		return out
	})
}

func (pr *Progress) Done(ds model.DatasetName) {
	pr.Swap(func(old Value) Value {
		out := make(Value, len(old)-1)
		for k, v := range old {
			out[k] = v
			if k == ds {
				process := out[k]
				process.isDone = true
			}
		}
		return out
	})
}
