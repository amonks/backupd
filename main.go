package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/user"
	"sort"

	"monks.co/backupd/model"
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

	args := flag.Args()
	if len(args) < 1 {
		return nil
	}
	cmd := args[0]

	b := New()

	var datasets []model.DatasetName
	switch *datasetArg {
	case "all", "":
		if err := b.RefreshState(ctx); err != nil {
			return err
		}

		datasets = b.state.ListDatasets()
		sort.Slice(datasets, func(i, j int) bool {
			return len(datasets[i]) < len(datasets[j])
		})

	case "root":
		if err := b.RefreshDataset(ctx, ""); err != nil {
			return err
		}

		datasets = []model.DatasetName{""}

	default:
		dataset := model.DatasetName(*datasetArg)
		if err := b.RefreshDataset(ctx, dataset); err != nil {
			return err
		}

		datasets = []model.DatasetName{dataset}
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
