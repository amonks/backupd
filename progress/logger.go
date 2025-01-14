package progress

import (
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

func (pr *Progress) Logger(ds model.DatasetName) *ProgressLogger {
	return &ProgressLogger{
		ds:     ds,
		logger: logger.New(ds.String()),
		pr:     pr,
	}
}

type ProgressLogger struct {
	ds     model.DatasetName
	logger logger.Logger
	pr     *Progress
}

func (pl *ProgressLogger) Printf(s string, args ...any) {
	pl.logger.Printf(s, args...)
	pl.pr.Log(pl.ds, s, args...)
}

func (pl *ProgressLogger) Done() {
	pl.pr.Done(pl.ds)
}
