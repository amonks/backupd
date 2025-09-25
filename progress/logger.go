package progress

import (
	"monks.co/backupd/logger"
)

// Logger returns a logger that writes to both standard log and ProcessLogs
func (pl *ProcessLogs) Logger(label string) *ProcessLogger {
	return &ProcessLogger{
		label: label,
		logs:  pl,
		inner: logger.New(label),
	}
}

type ProcessLogger struct {
	label string
	logs  *ProcessLogs
	inner logger.Logger
}

func (pl *ProcessLogger) Write(bs []byte) (int, error) {
	return pl.inner.Write(bs)
}

func (pl *ProcessLogger) Printf(s string, args ...any) {
	pl.inner.Printf(s, args...)
	pl.logs.Log(s, args...)
}
