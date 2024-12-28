package main

import (
	"context"
	"os/exec"

	"monks.co/backupbot/env"
)

func main() {
	ls := exec.Command("tail", "-f", "log.log")
	wc := exec.Command("wc", "-l")
	if err := env.Pipe(context.Background(), ls, wc); err != nil {
		panic(err)
	}
}
