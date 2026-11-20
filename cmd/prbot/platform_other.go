//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func isService() bool { return false }

// runPlatform runs until SIGINT/SIGTERM (systemd sends SIGTERM on stop).
func runPlatform(run func(ctx context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}
