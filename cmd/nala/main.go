package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"guarde.sh/nala/internal/api"
	"guarde.sh/nala/internal/auth"
	"guarde.sh/nala/internal/config"
	"guarde.sh/nala/internal/perms"
	"guarde.sh/nala/internal/sandbox"
	"guarde.sh/nala/internal/store"
	"guarde.sh/nala/internal/worker"
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
		return errors.New("usage: nala <migrate|server|worker|all|create-admin>")
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
	case "create-admin":
		return createAdmin(ctx, st, os.Args[2:])
	case "server", "worker", "all":
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		start := func(fn func(context.Context) error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := fn(ctx); err != nil {
					errs <- err
					stop()
				}
			}()
		}
		if cmd != "worker" {
			start(func(ctx context.Context) error { return serve(ctx, st, cfg, log) })
		}
		if cmd != "server" {
			start(func(ctx context.Context) error { return runWorker(ctx, st, cfg, log) })
		}
		wg.Wait()
		close(errs)
		return <-errs
	default:
		return fmt.Errorf("Unknown command: %q", cmd)
	}
}

func runWorker(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	sb := cfg.Sandbox
	runner := sandbox.Docker{
		Bin:       sb.DockerBinary,
		Runtime:   sb.Runtime,
		Memory:    sb.Memory,
		CPUs:      sb.CPUs,
		PidsLimit: sb.PidsLimit,
		TmpfsSize: sb.TmpfsSize,
		User:      sb.User,
	}
	return worker.New(st, runner, cfg.Worker, cfg.AllowedImages, log).Run(ctx)
}

func serve(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           api.New(st, cfg, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
					log.Error("delete expired sessions", "err", err)
				}
			}
		}
	}()

	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.Address)
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	log.Info("api shutting down")
	return srv.Shutdown(shutdownCtx)
}

func createAdmin(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: NALA_ADMIN_PASSWORD=... nala create-admin <email>")
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	pw := os.Getenv("NALA_ADMIN_PASSWORD")
	if err := auth.CheckPasswordPolicy(pw); err != nil {
		return fmt.Errorf("NALA_ADMIN_PASSWORD: %w", err)
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	u, err := st.CreateUser(ctx, email, hash, perms.PlatformAdmin)
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("a user with email %s already exists", email)
	}
	if err != nil {
		return err
	}
	fmt.Printf("created platform admin %s (%s)\n", u.Email, u.ID)
	return nil
}
