package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func NewSigctx() context.Context {
	ctx, _ := NewSigctxWithCancel()
	return ctx
}

func NewSigctxWithCancel() (context.Context, func(err error)) {
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
		sig := <-sigs
		cancel(fmt.Errorf("got signal: %s", sig))
	}()
	return ctx, cancel
}
