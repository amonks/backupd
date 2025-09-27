package logger

import (
	"fmt"
	"io"
	"log"
	"time"
)

type Logger struct {
	label string
	logs  []LogEntry
}

type LogEntry struct {
	LogAt time.Time
	Log   string
}

var _ io.Writer = &Logger{}

func New(label string) *Logger {
	return &Logger{
		label: label,
		logs:  []LogEntry{},
	}
}

func (p *Logger) Printf(s string, args ...any) {
	p.Write([]byte(fmt.Sprintf(s, args...)))
}

func (p *Logger) Write(bs []byte) (int, error) {
	entry := LogEntry{
		LogAt: time.Now(),
		Log:   string(bs),
	}
	p.logs = append(p.logs, entry)
	log.Println(fmt.Sprintf("[%s]\t", p.label) + string(bs))
	return len(bs), nil
}

func (p *Logger) GetLogs() []LogEntry {
	return p.logs
}
