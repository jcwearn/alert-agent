// Command alert-agent is one binary with several roles: the long-running
// controller, its schema migration, and (from Phase 2) the runner that wraps
// `claude -p` inside a per-incident Job.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/jcwearn/alert-agent/internal/controller"
	"github.com/jcwearn/alert-agent/internal/store"
)

const usage = "usage: alert-agent <controller|migrate|runner>"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "controller":
		err = runController(ctx)
	case "migrate":
		err = runMigrate(ctx)
	case "runner":
		err = errors.New("runner: not implemented yet")
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	if err != nil {
		slog.Error("exiting", "err", err)
		return 1
	}
	return 0
}

func runController(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	return controller.New(st).Run(ctx, envOr("LISTEN_ADDR", ":8080"), envOr("WEBHOOK_LISTEN_ADDR", ":8081"))
}

func runMigrate(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

// openStore builds the connection string from the pieces CloudNativePG puts
// in its generated app Secret, so no password ever sits in a URL in a manifest.
func openStore(ctx context.Context) (*store.Store, error) {
	host, name, user, pass := os.Getenv("DB_HOST"), os.Getenv("DB_NAME"), os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD")
	if host == "" || name == "" || user == "" {
		return nil, errors.New("DB_HOST, DB_NAME and DB_USER must be set")
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     host,
		Path:     "/" + name,
		RawQuery: "sslmode=" + envOr("DB_SSLMODE", "require"),
	}
	return store.Open(ctx, u.String())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
