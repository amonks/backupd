package main

import (
	"context"
	"os/exec"

	"monks.co/backupd/env"
)

func main() {
	ls := exec.Command("tail", "-f", "log.log")
	wc := exec.Command("wc", "-l")
	if err := env.Pipe(context.Background(), "pipetest", ls, wc); err != nil {
		panic(err)
	}
}
