package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/user"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"monks.co/backupd/config"
	"monks.co/backupd/logger"
	"monks.co/backupd/model"
)

func main() {
	defer func() {
		if err := recover(); err != nil {
			log.Fatalf("panic: %v", err)
		}
	}()
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

	config, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	var (
		debugDS string
		logfile string
		addr    string
		dryrun  bool
	)
	flag.StringVar(&debugDS, "debug", "", "debug a dataset")
	flag.StringVar(&logfile, "logfile", "", "log to a file")
	flag.StringVar(&addr, "addr", "0.0.0.0:8888", "server addr")
	flag.BoolVar(&dryrun, "dryrun", false, "refresh state but don't transfer or delete snapshots")
	flag.Parse()

	ctx := NewSigctx()
	b := New(config, addr, dryrun)

	if debugDS != "" {
		logger := logger.New("refresh")
		ds := model.DatasetName(debugDS)
		if err := b.refreshDataset(ctx, logger, ds); err != nil {
			return err
		} else if err := b.Plan(ctx, ds); err != nil {
			return err
		}
		return nil
	}

	if logfile != "" {
		logger := &lumberjack.Logger{
			Filename:   logfile,
			MaxSize:    15,
			MaxBackups: 3,
			MaxAge:     28,
		}
		defer logger.Close()
		log.SetOutput(logger)
	}

	if err := b.Go(ctx); err != nil {
		return err
	}

	return nil
}
