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

	// Customize the help output (after flags are defined)
	flag.Usage = func() {
		fmt.Println("backupd - ZFS snapshot backup daemon")
		fmt.Println()
		fmt.Println("USAGE:")
		fmt.Println("    backupd [OPTIONS]                    # Start backup daemon")
		fmt.Println("    backupd snapshot <periodicity>      # Create snapshot and update state")
		fmt.Println()
		fmt.Println("EXAMPLES:")
		fmt.Println("    backupd snapshot daily     # Create daily snapshot")
		fmt.Println("    backupd snapshot monthly   # Create monthly snapshot")
		fmt.Println("    backupd snapshot yearly    # Create yearly snapshot")
		fmt.Println()
		fmt.Println("OPTIONS:")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Handle subcommands
	args := flag.Args()
	if len(args) > 0 {
		switch args[0] {
		case "snapshot":
			if len(args) != 2 {
				return fmt.Errorf("usage: backupd snapshot <periodicity>")
			}
		default:
			return fmt.Errorf("unknown command: %s\nRun 'backupd --help' for usage information", args[0])
		}
	}

	// Root check (after help handling)
	if whoami, err := user.Current(); err != nil {
		return fmt.Errorf("getting user: %w", err)
	} else if whoami.Username != "root" {
		return fmt.Errorf("must be root, not '%s'", whoami)
	}

	config, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx := NewSigctx()
	b := New(config, addr, dryrun)

	// Execute subcommands
	if len(args) > 0 {
		switch args[0] {
		case "snapshot":
			return b.CreateSnapshot(ctx, args[1])
		}
	}

	if debugDS != "" {
		if debugDS == "<root>" {
			debugDS = ""
		}
		logger := b.globalLogs
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
