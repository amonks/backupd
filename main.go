package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	b := New()

	if err := b.Go(ctx); err != nil {
		return err
	}

	return nil
}
