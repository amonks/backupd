package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/user"
	"sort"

	"monks.co/backupbot/db"
	"monks.co/backupbot/model"
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

	datasetArg := flag.String("dataset", "", "specify dataset; all by default")
	flag.Parse()

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
		return nil
	}
	cmd := args[0]

	switch cmd {
	case "refresh":
		return b.RefreshState(ctx)
	}

	var datasets []model.DatasetName
	switch *datasetArg {
	case "":
		return fmt.Errorf("must specify dataset flag or 'all'")
	case "root":
		datasets = []model.DatasetName{""}
	case "all":
		for ds := range b.state.Datasets {
			datasets = append(datasets, ds)
		}
		sort.Slice(datasets, func(i, j int) bool {
			return len(datasets[i]) < len(datasets[j])
		})
	default:
		datasets = []model.DatasetName{model.DatasetName(*datasetArg)}
	}

	for _, ds := range datasets {
		fmt.Printf("======== %s ========\n", ds)

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

		fmt.Println()
	}
	return nil
}
