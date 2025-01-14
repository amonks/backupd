package main

import (
	"context"
	"os/exec"

	"monks.co/backupd/env"
	"monks.co/backupd/logger"
)

func main() {
	ls := exec.Command("tail", "-f", "log.log")
	wc := exec.Command("wc", "-l")
	if err := env.Pipe(context.Background(), logger.New("pipetest"), ls, wc); err != nil {
		panic(err)
	}
}
