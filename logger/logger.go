package logger

import "log"

type Logger interface {
	Printf(s string, args ...any)
}

type logger struct{ label string }

func New(label string) Logger {
	return logger{label}
}

func (l logger) Printf(s string, args ...any) {
	args = append([]any{string(l.label)}, args...)
	log.Printf("[%s]\t"+s, args...)
}
