package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/server"
	"github.com/stackwright/stackwright/internal/version"
)

func runServer(args []string) error {
	fl := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fl.String("listen", ":8080", "address to serve on")
	webDir := fl.String("web", "", "folder with the built web app")
	_ = fl.Parse(args)

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := dbConfig()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	var web fs.FS
	if *webDir != "" {
		web = os.DirFS(*webDir)
	}
	handler := server.New(pool, web)

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return fmt.Errorf("start tracing: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
		defer func() { _ = tp.Shutdown(context.Background()) }()
		otel.SetTracerProvider(tp)
		handler = otelhttp.NewHandler(handler, "http")
	}

	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("server started", "listen", *listen, "version", version.Version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func dbConfig() (db.Config, error) {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	port, err := strconv.Atoi(get("STACKWRIGHT_POSTGRES_PORT", "5432"))
	if err != nil {
		return db.Config{}, fmt.Errorf("STACKWRIGHT_POSTGRES_PORT: %w", err)
	}
	password := os.Getenv("STACKWRIGHT_POSTGRES_PASSWORD")
	if file := os.Getenv("STACKWRIGHT_POSTGRES_PASSWORD_FILE"); file != "" {
		b, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return db.Config{}, fmt.Errorf("STACKWRIGHT_POSTGRES_PASSWORD_FILE: %w", err)
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	return db.Config{
		Host:     get("STACKWRIGHT_POSTGRES_HOST", "postgres"),
		Port:     port,
		User:     get("STACKWRIGHT_POSTGRES_USER", "stackwright"),
		Database: get("STACKWRIGHT_POSTGRES_DB", "stackwright"),
		Password: password,
		SSLMode:  get("STACKWRIGHT_POSTGRES_SSLMODE", "prefer"),
	}, nil
}
