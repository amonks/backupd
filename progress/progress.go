package progress

import (
	"fmt"
	"time"

	"monks.co/backupd/atom"
	"monks.co/backupd/model"
)

type Progress struct {
	*atom.Atom[Value]
}

type Value = map[model.DatasetName][]LogEntry

type LogEntry struct {
	LogAt time.Time
	Log   string
}

func New() *Progress {
	return &Progress{
		atom.New(Value{}),
	}
}

func (pr *Progress) Deref() map[model.DatasetName][]LogEntry {
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
			} else {
				seen = true
				out[k] = append(v, entry)
			}
		}
		if !seen {
			out[ds] = []LogEntry{entry}
		}
		return out
	})
}

func (pr *Progress) Done(ds model.DatasetName) {
	pr.Swap(func(old Value) Value {
		out := make(Value, len(old)-1)
		for k, v := range old {
			if k != ds {
				out[k] = v
			}
		}
		return out
	})
}
