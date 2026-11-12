package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/DR1N0/lanlobby-cli/internal/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Options{Output: os.Stdout, Diagnostics: os.Stderr})
	cancel()
	os.Exit(code)
}
