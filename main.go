package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/user"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"monks.co/backupd/model"
)

const logpath = `/var/log/backupd.log`

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		log.Fatalf("error: %v", err)
	}
}

func run() error {
	if whoami, err := user.Current(); err != nil {
		return fmt.Errorf("getting user: %w", err)
	} else if whoami.Username != "root" {
		return fmt.Errorf("must be root, not '%s'", whoami)
	}

	ctx := NewSigctx()
	b := New()

	var debugDS string
	flag.StringVar(&debugDS, "debug", "", "debug a dataset")
	flag.Parse()

	if debugDS != "" {
		if err := b.refreshState(ctx); err != nil {
			return err
		} else if err := b.Plan(ctx, model.DatasetName(debugDS)); err != nil {
			return err
		}
		return nil
	}

	logger := &lumberjack.Logger{
		Filename:   logpath,
		MaxSize:    15,
		MaxBackups: 3,
		MaxAge:     28,
	}
	defer logger.Close()
	log.SetOutput(logger)

	if err := b.Go(ctx); err != nil {
		return err
	}

	return nil
}
