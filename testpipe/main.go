package main

import (
	"context"
	"os/exec"

	"monks.co/backupd/env"
	"monks.co/backupd/logger"
)

func main() {
	ls := exec.Command("fish", "-c", "while true ; echo hello world ; sleep 0.1 ; end")
	// ls := exec.Command("cat", "/var/log/backupd.log")
	wc := exec.Command("awk", "{ print $1 }")
	if err := env.Pipe(context.Background(), logger.New("pipetest"), 0, ls, wc); err != nil {
		panic(err)
	}
}
