package main

import (
	"fmt"
	"os/exec"
	"strings"
)

var _ Executor = LocalExecutor{}
var Local = LocalExecutor{}

type LocalExecutor struct{}

func (_ LocalExecutor) Exec(args ...string) ([]string, error) {
	return Exec(args...)
}

func (_ LocalExecutor) Execf(s string, args ...any) ([]string, error) {
	return Execf(s, args...)
}

func Exec(args ...string) ([]string, error) {
	name, args := args[0], args[1:]
	cmd := exec.Command(name, args...)
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return strings.Split(out.String(), "\n"), nil
}

func Execf(s string, args ...any) ([]string, error) {
	return Exec(strings.Fields(fmt.Sprintf(s, args...))...)
}
