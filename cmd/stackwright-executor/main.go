// stackwright-executor runs one job inside a sandbox, then exits.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/stackwright/stackwright/internal/executor"
	"github.com/stackwright/stackwright/internal/sandbox"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverURL := os.Getenv("STACKWRIGHT_SERVER_URL")
	ticketFile := filepath.Join(sandbox.FilesDir, "ticket")
	ticket, err := os.ReadFile(ticketFile) //nolint:gosec // a fixed path
	if err != nil || serverURL == "" {
		slog.Error("the sandbox needs STACKWRIGHT_SERVER_URL and a ticket file", "ticket", ticketFile, "err", err)
		os.Exit(2)
	}
	if err := os.Remove(ticketFile); err != nil {
		slog.Warn("remove the ticket file", "err", err)
	}
	if err := executor.Run(ctx, serverURL, string(ticket), "/work"); err != nil {
		slog.Error("job failed", "err", err)
		os.Exit(1)
	}
}
