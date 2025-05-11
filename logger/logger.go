package logger

import (
	"io"
	"log"
)

type Logger interface {
	io.Writer
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

var _ io.Writer = logger{}

func (l logger) Write(bs []byte) (int, error) {
	log.Println(string(bs))
	return len(bs), nil
}
