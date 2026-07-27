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
		simMode bool
	)

	flag.StringVar(&debugDS, "debug", "", "debug a dataset")
	flag.StringVar(&logfile, "logfile", "", "log to a file")
	flag.StringVar(&addr, "addr", "", "server addr (overrides the config 'listen' setting; default 0.0.0.0:8888)")
	flag.BoolVar(&dryrun, "dryrun", false, "refresh state but don't transfer or delete snapshots")
	flag.BoolVar(&simMode, "sim", false, "run the full daemon against a simulated ZFS pair (no root or ZFS required)")

	// Customize the help output (after flags are defined)
	flag.Usage = func() {
		fmt.Println("backupd - ZFS snapshot backup daemon")
		fmt.Println()
		fmt.Println("USAGE:")
		fmt.Println("    backupd [OPTIONS]                    # Start backup daemon")
		fmt.Println("    backupd snapshot <periodicity>      # Create snapshot and update state")
		fmt.Println("    backupd pause [dataset]             # Pause execution (all, or one subtree)")
		fmt.Println("    backupd resume [dataset]            # Resume execution (all, or one subtree)")
		fmt.Println("    backupd sync [dataset]              # Sync now (full cycle, or one dataset)")
		fmt.Println()
		fmt.Println("EXAMPLES:")
		fmt.Println("    backupd snapshot daily     # Create daily snapshot")
		fmt.Println("    backupd pause /tm          # Pause the /tm subtree")
		fmt.Println("    backupd sync               # Start a sync cycle immediately")
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
		case "pause", "resume", "sync":
			if len(args) > 2 {
				return fmt.Errorf("usage: backupd %s [dataset]", args[0])
			}
		default:
			return fmt.Errorf("unknown command: %s\nRun 'backupd --help' for usage information", args[0])
		}
	}

	// Sim mode: the whole daemon against an in-memory environment; no
	// root, config file, or ZFS needed.
	if simMode {
		return runSim(NewSigctx(), addr)
	}

	// Root check (after help handling)
	if whoami, err := user.Current(); err != nil {
		return fmt.Errorf("getting user: %w", err)
	} else if whoami.Username != "root" {
		return fmt.Errorf("must be root, not '%s'", whoami)
	}

	conf, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// The -addr flag wins over the config's listen setting.
	if addr == "" {
		addr = conf.Listen
	}
	if addr == "" {
		addr = "0.0.0.0:8888"
	}

	ctx := NewSigctx()
	b := New(conf, addr, dryrun)

	// Execute subcommands
	if len(args) > 0 {
		switch args[0] {
		case "snapshot":
			return b.CreateSnapshot(ctx, args[1])
		case "pause", "resume", "sync":
			path := "/api/" + args[0]
			if len(args) == 2 {
				path += "?dataset=" + args[1]
			}
			return b.CallAPI(ctx, "POST", path)
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
