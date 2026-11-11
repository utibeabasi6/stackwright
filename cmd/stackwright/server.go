package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/seal"
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

	cfg, err := dbConfig("stackwright_app", "STACKWRIGHT_APP_DB_PASSWORD")
	if err != nil {
		return err
	}
	kekText, err := secretEnv("STACKWRIGHT_KEK")
	if err != nil {
		return err
	}
	kek, err := seal.ParseKEK(kekText)
	if err != nil {
		return fmt.Errorf("STACKWRIGHT_KEK: %w", err)
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	var setupCode string
	var needed bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT FROM users)`).Scan(&needed); err != nil {
		return fmt.Errorf("check setup: %w", err)
	}
	if needed {
		setupCode = strings.ToLower(rand.Text()[:8])
		slog.Warn("this instance has no admin yet; open it in a browser and enter the setup code", "setup_code", setupCode)
	}

	var web fs.FS
	if *webDir != "" {
		web = os.DirFS(*webDir)
	}
	handler := server.New(server.Config{
		Pool:      pool,
		KEK:       kek,
		PublicURL: strings.TrimRight(env("STACKWRIGHT_PUBLIC_URL", "http://127.0.0.1:8080"), "/"),
		SetupCode: setupCode,
		Web:       web,
	})

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
