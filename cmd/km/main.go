// Command km is the entry point of the Kali-Mac CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"kalimac/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
