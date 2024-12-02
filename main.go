package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		panic(err)
	}
}

func run() error {
	if whoami, err := user.Current(); err != nil {
		return fmt.Errorf("getting user: %w", err)
	} else if whoami.Username != "root" {
		return fmt.Errorf("must be root, not '%s'", whoami)
	}

	ctx := NewSigctx()

	flag.CommandLine.Init("backup", flag.ContinueOnError)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "\nmanage zfs backups\n\n")
		fmt.Fprintf(os.Stderr, "  backup [flags] $cmd <defined by cmd...>\n\n")
		fmt.Fprintf(os.Stderr, "flags:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "  $cmd {...}\n")
		fmt.Fprintf(os.Stderr, "  \twhich command to run\n")
		fmt.Fprintf(os.Stderr, "  \trun `flag $cmd -help` for more details about each command.\n")
	}
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		return fmt.Errorf("flag parse error: %w", err)
	} else if err != nil {
		return nil
	}

	db, err := OpenDB("/data/tank/backup-info/backup-info.db")
	if err != nil {
		return fmt.Errorf("opening backup info db: %w", err)
	}
	defer db.Close()

	b := NewBackup(
		db,
		NewZFS("data/tank", Local),
		NewZFS(
			"data1/thor/tank",
			NewRemote(
				"/home/ajm/.ssh/id_ed25519",
				"root@57269.zfs.rsync.net",
			),
		),
	)
	if err := b.ObserveDatasets(ctx); err != nil {
		return err
	}

	args := flag.Args()
	if len(args) < 1 {
		flag.CommandLine.Parse([]string{"-help"})
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "backup":
		return CLIBackup(ctx, b, args)
	case "observe":
		return CLIObserve(ctx, b, args)
	case "prune":
		return CLIPrune(ctx, b, args)
	default:
		return fmt.Errorf("unknown cmd: '%s'", cmd)
	}
}

func CLIObserve(ctx context.Context, b *Backup, args []string) error {
	subcmd := NewSubcmd("observe", "refresh remote and local state")
	var (
		dataset = subcmd.String("dataset", "", "specify a particular dataset, or leave blank for all datasets")
	)
	if err := subcmd.Parse(args); err != nil {
		return fmt.Errorf("flag parsing err: %w", err)
	}

	var todo []string
	if dataset != nil && *dataset != "" {
		todo = []string{*dataset}
	} else {
		datasets, err := b.q.GetLocalDatasets(ctx)
		if err != nil {
			return fmt.Errorf("getting datasets: %w", err)
		}
		todo = make([]string, len(datasets))
		for i, ds := range datasets {
			todo[i] = ds.Name
		}
	}

	for _, name := range todo {
		dataset, err := b.q.GetDataset(ctx, name)
		if err != nil {
			return fmt.Errorf("getting dataset %s: %w", name, err)
		}
		b.ObserveSnapshots(ctx, dataset)
	}

	return nil
}

func CLIBackup(ctx context.Context, b *Backup, args []string) error {
	subcmd := NewSubcmd("backup", "backup datasets")
	var (
	// dataset = subcmd.String("dataset", "", "specify a particular dataset, or leave blank for all datasets")
	)
	if err := subcmd.Parse(args); err != nil {
		return fmt.Errorf("flag parsing err: %w", err)
	}

	return nil
}

func CLIPrune(ctx context.Context, b *Backup, args []string) error {
	subcmd := NewSubcmd("backup", "backup datasets")
	var (
		dataset = subcmd.String("dataset", "", "specify a particular dataset, or leave blank for all datasets")
	)
	if err := subcmd.Parse(args); err != nil {
		return fmt.Errorf("flag parsing err: %w", err)
	}

	var todo []string
	if dataset != nil && *dataset != "" {
		todo = []string{*dataset}
	} else {
		datasets, err := b.q.GetLocalDatasets(ctx)
		if err != nil {
			return fmt.Errorf("getting datasets: %w", err)
		}
		todo = make([]string, len(datasets))
		for i, ds := range datasets {
			todo[i] = ds.Name
		}
	}

	for _, dataset := range todo {
		if err := b.Prune(ctx, dataset); err != nil {
			return err
		}
	}

	return nil
}
