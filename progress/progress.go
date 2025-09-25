package progress

import (
	"fmt"
	"time"
)

type ProcessLogs struct {
	logs []LogEntry
}

type LogEntry struct {
	LogAt time.Time
	Log   string
}

func NewProcessLogs() *ProcessLogs {
	return &ProcessLogs{
		logs: []LogEntry{},
	}
}

func (p *ProcessLogs) Log(s string, args ...any) {
	entry := LogEntry{
		LogAt: time.Now(),
		Log:   fmt.Sprintf(s, args...),
	}
	p.logs = append(p.logs, entry)
}

func (p *ProcessLogs) GetLogs() []LogEntry {
	return p.logs
}
