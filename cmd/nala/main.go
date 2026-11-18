package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"guarde.sh/nala/internal/config"
	"guarde.sh/nala/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) < 2 {
		// todo(@guarde): create dynamic help list
		return errors.New("usage: nala <migrate|server|worker|all>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	switch cmd := os.Args[1]; cmd {
	case "migrate":
		return st.Migrate(ctx)
	case "server", "worker", "all":
		return fmt.Errorf("not yet implemeneted")
	default:
		return fmt.Errorf("Unknown command: %q", cmd)
	}
}
