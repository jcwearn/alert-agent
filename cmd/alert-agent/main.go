// Command alert-agent is one binary with two roles: the long-running
// controller, and the runner that wraps `claude -p` inside a per-incident Job.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jcwearn/alert-agent/internal/controller"
)

const usage = "usage: alert-agent <controller|runner>"

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
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		err = controller.Run(ctx, addr)
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
