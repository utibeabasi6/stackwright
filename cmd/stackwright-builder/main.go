// stackwright-builder starts sandboxes for the control plane (adr/0007,
// adr/0021).
//
//	stackwright-builder enrol --server URL --ca-sha256 HEX   reads a token on stdin
//	stackwright-builder install                             installs the systemd service
//	stackwright-builder [run]                               runs the builder
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/stackwright/stackwright/internal/builder"
	"github.com/stackwright/stackwright/internal/config"
	"github.com/stackwright/stackwright/internal/sandbox"
	"github.com/stackwright/stackwright/internal/version"
)

const (
	stateFile = "/var/lib/stackwright-builder/identity.json"
	cacheDir  = "/var/cache/stackwright-builder"
	unitFile  = "/etc/systemd/system/stackwright-builder.service"
	binary    = "/usr/local/bin/stackwright-builder"
)

const unit = `[Unit]
Description=Stackwright build server
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service

[Service]
ExecStart=` + binary + ` run
Restart=always
RestartSec=5
StateDirectory=stackwright-builder
CacheDirectory=stackwright-builder

[Install]
WantedBy=multi-user.target
`

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "version":
		fmt.Println(version.Version)
	case "enrol":
		err = enrol(args)
	case "install":
		err = install()
	case "run":
		err = run()
	default:
		err = fmt.Errorf("unknown command %q; use enrol, install, run or version", cmd)
	}
	if err != nil {
		slog.Error("builder stopped", "command", cmd, "err", err)
		os.Exit(1)
	}
}

func enrol(args []string) error {
	fl := flag.NewFlagSet("enrol", flag.ExitOnError)
	server := fl.String("server", "", "the builder URL, such as https://stackwright.example.com:8443")
	pin := fl.String("ca-sha256", "", "the SHA-256 of the builder CA, from the enrol page")
	state := fl.String("state", stateFile, "where to keep the identity")
	_ = fl.Parse(args)
	token, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && token == "" {
		return errors.New("write the enrol token to stdin")
	}
	client, err := builder.PinnedClient(*server, strings.ToLower(*pin))
	if err != nil {
		return err
	}
	id, err := builder.Enrol(context.Background(), client, strings.TrimSpace(token), 0, nil, *state, *server)
	if err != nil {
		return err
	}
	slog.Info("enrolled", "name", id.Name, "build_server_id", id.ServerID, "state", *state)
	return nil
}

func install() error {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		return errors.New("no Docker on this host: install Docker Engine (https://docs.docker.com/engine/install/), then run this again")
	}
	if _, err := os.Stat(stateFile); err != nil {
		return fmt.Errorf("run `stackwright-builder enrol` first: %w", err)
	}
	if err := os.WriteFile(unitFile, []byte(unit), 0o644); err != nil { //nolint:gosec // systemd reads unit files as root
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "stackwright-builder"}, {"restart", "stackwright-builder"}} {
		if out, err := exec.CommandContext(context.Background(), "systemctl", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed arguments
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	slog.Info("the stackwright-builder service is running")
	return nil
}

// run starts the builder. With STACKWRIGHT_BUILDER_TOKEN set it is the
// built-in builder: it enrols at each start, over the server's own port
// inside compose, and keeps its identity in memory.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var join []string
	if j := config.Env("STACKWRIGHT_SANDBOX_JOIN", ""); j != "" {
		join = strings.Split(j, ",")
	}
	cache := config.Env("STACKWRIGHT_BUILDER_CACHE", cacheDir)
	docker, err := sandbox.NewDocker(sandbox.DockerConfig{Join: join, Cache: config.Env("STACKWRIGHT_SANDBOX_CACHE", cache)})
	if err != nil {
		return err
	}

	var id *builder.Identity
	if token, _ := config.Secret("STACKWRIGHT_BUILDER_TOKEN"); token != "" {
		slots, err := strconv.ParseInt(config.Env("STACKWRIGHT_BUILDER_SLOTS", "2"), 10, 32)
		if err != nil {
			return fmt.Errorf("STACKWRIGHT_BUILDER_SLOTS: %w", err)
		}
		labels := map[string]string{}
		if l := config.Env("STACKWRIGHT_BUILDER_LABELS", ""); l != "" {
			for _, kv := range strings.Split(l, ",") {
				k, v, _ := strings.Cut(kv, "=")
				labels[k] = v
			}
		}
		client := builder.PlainClient(config.Env("STACKWRIGHT_SERVER_URL", "http://stackwright:8080"))
		if id, err = builder.Enrol(ctx, client, token, int32(slots), labels, "", config.Env("STACKWRIGHT_BUILDER_URL", "https://stackwright:8443")); err != nil {
			return err
		}
	} else if id, err = builder.Load(config.Env("STACKWRIGHT_BUILDER_STATE", stateFile)); err != nil {
		return fmt.Errorf("load the identity; run `stackwright-builder enrol` first: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "engines"), 0o755); err != nil { //nolint:gosec // sandboxes read the cache as another user
		return err
	}
	b := &builder.Builder{Identity: id, Version: version.Version, Backend: docker, BackendName: "docker", Cache: cache}
	b.Run(ctx)
	return nil
}
