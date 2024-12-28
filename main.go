package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"sort"

	"monks.co/backupbot/db"
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
	datasetArg := flag.CommandLine.String("dataset", "", "specify dataset; all by default")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		return fmt.Errorf("flag parse error: %w", err)
	} else if err != nil {
		return nil
	}

	db, err := db.Open("/data/tank/backup-info/backup-info.db")
	if err != nil {
		return fmt.Errorf("opening backup info db: %w", err)
	}
	defer db.Close()

	b := New(db)
	if err := b.LoadState(ctx); err != nil {
		return err
	}

	args := flag.Args()
	if len(args) < 1 {
		flag.CommandLine.Parse([]string{"-help"})
		return nil
	}
	cmd := args[0]

	var datasets []string
	if *datasetArg != "" {
		datasets = []string{*datasetArg}
	} else {
		for ds := range b.state.Datasets {
			datasets = append(datasets, ds)
		}
		sort.Slice(datasets, func(i, j int) bool {
			return len(datasets[i]) < len(datasets[j])
		})
	}

	switch cmd {
	case "refresh":
		return b.RefreshState(ctx)
	}

	for _, ds := range datasets {
		log.Printf("==== %s ====", ds)

		switch cmd {
		case "plan":
			if err := b.Plan(ctx, ds); err != nil {
				return err
			}
		case "go":
			if err := b.Go(ctx, ds); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported cmd '%s'", cmd)
		}
	}
	return nil
}
