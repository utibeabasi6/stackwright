// stackwright-executor runs one job inside a sandbox, then exits. With the
// argument "toolbox", it serves an agent session instead.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stackwright/stackwright/internal/executor"
	"github.com/stackwright/stackwright/internal/sandbox"
	"github.com/stackwright/stackwright/internal/toolbox"
	"github.com/stackwright/stackwright/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.Version)
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cache := ""
	if fi, err := os.Stat(sandbox.CacheDir); err == nil && fi.IsDir() {
		cache = sandbox.CacheDir
	}
	if len(os.Args) == 2 && os.Args[1] == "mirror" {
		if err := mirror(ctx, cache); err != nil {
			slog.Error("mirror providers", "err", err)
			os.Exit(1)
		}
		return
	}

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
	if len(os.Args) == 2 && os.Args[1] == "toolbox" {
		if err := toolbox.Run(ctx, serverURL, string(ticket), "/work", cache); err != nil {
			slog.Error("toolbox stopped", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := executor.Run(ctx, serverURL, string(ticket), "/work", cache); err != nil {
		slog.Error("job failed", "err", err)
		os.Exit(1)
	}
}

// mirror reads its engine as "sha256 name" and its providers one a line.
func mirror(ctx context.Context, cache string) error {
	if cache == "" {
		return fmt.Errorf("no cache at %s", sandbox.CacheDir)
	}
	engine, err := os.ReadFile(filepath.Join(sandbox.FilesDir, "engine"))
	if err != nil {
		return err
	}
	providers, err := os.ReadFile(filepath.Join(sandbox.FilesDir, "providers"))
	if err != nil {
		return err
	}
	sha, name, ok := strings.Cut(strings.TrimSpace(string(engine)), " ")
	if !ok {
		return fmt.Errorf("engine file %q is not \"sha256 name\"", engine)
	}
	path := filepath.Join(cache, "engines", filepath.Base(sha), filepath.Base(name))
	return executor.Mirror(ctx, path, sha, strings.Split(strings.TrimSpace(string(providers)), "\n"), cache, "/work")
}
