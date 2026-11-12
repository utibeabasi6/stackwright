// stackwright-builder starts sandboxes for the control plane (adr/0007).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/stackwright/stackwright/internal/builder"
	"github.com/stackwright/stackwright/internal/config"
	"github.com/stackwright/stackwright/internal/sandbox"
	"github.com/stackwright/stackwright/internal/version"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("builder stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token, err := config.Secret("STACKWRIGHT_BUILDER_TOKEN")
	if err != nil {
		return err
	}
	slots, err := strconv.ParseInt(config.Env("STACKWRIGHT_BUILDER_SLOTS", "2"), 10, 32)
	if err != nil {
		return fmt.Errorf("STACKWRIGHT_BUILDER_SLOTS: %w", err)
	}
	var join []string
	if j := config.Env("STACKWRIGHT_SANDBOX_JOIN", ""); j != "" {
		join = strings.Split(j, ",")
	}
	docker, err := sandbox.NewDocker(sandbox.DockerConfig{Join: join})
	if err != nil {
		return err
	}
	serverURL := config.Env("STACKWRIGHT_SERVER_URL", "http://stackwright:8080")
	b := &builder.Builder{
		Client:    builder.Dial(serverURL),
		Token:     token,
		Name:      config.Env("STACKWRIGHT_BUILDER_NAME", "built-in"),
		Slots:     int32(slots),
		Version:   version.Version,
		Backend:   docker,
		Image:     config.Env("STACKWRIGHT_EXECUTOR_IMAGE", "stackwright/executor:"+version.Version),
		ServerURL: config.Env("STACKWRIGHT_SANDBOX_SERVER_URL", serverURL),
	}
	b.Run(ctx)
	return nil
}
