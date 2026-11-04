package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/liquidcatmofu/mcwarden/internal/bootstrap"
	"github.com/liquidcatmofu/mcwarden/internal/cli"
)

func main() {
	// SIGTERM is the normal shutdown request from systemd. It is converted to
	// context cancellation only; the long-running agent never forwards it to
	// the Minecraft runtime.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, bootstrap.Service))
}
