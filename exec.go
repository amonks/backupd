package main

import (
	"fmt"
	"log"
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
	log.Printf("run %s {%s}", name, strings.Join(args, ", "))
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.Join(strings.Split(strings.TrimSpace(string(out)), "\n"), "; ")
		return nil, fmt.Errorf("running '%s': %w: %s", name, err, output)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

func Execf(s string, args ...any) ([]string, error) {
	return Exec(strings.Fields(fmt.Sprintf(s, args...))...)
}
