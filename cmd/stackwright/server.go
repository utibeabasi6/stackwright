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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/config"
	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/engine"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/runs"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/server"
	"github.com/stackwright/stackwright/internal/state"
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
	kekText, err := config.Secret("STACKWRIGHT_KEK")
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

	s3, presign, bucket, err := objectStore(ctx)
	if err != nil {
		return err
	}
	natsPassword, err := config.Secret("STACKWRIGHT_NATS_PASSWORD")
	if err != nil {
		return err
	}
	nc, err := nats.Connect(config.Env("STACKWRIGHT_NATS_URL", "nats://nats:4222"), nats.UserInfo("stackwright", natsPassword),
		nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Close()
	builderToken, err := config.Secret("STACKWRIGHT_BUILDER_TOKEN")
	if err != nil {
		return err
	}

	jobService := &jobs.Service{
		Pool: pool, KEK: kek, S3: s3, Presign: presign, Bucket: bucket,
		Engines: &engine.Mirror{Pool: pool, S3: s3, Bucket: bucket, HTTP: &http.Client{Timeout: 10 * time.Minute}},
	}
	hub := &builders.Hub{}
	orchestrator := &runs.Orchestrator{Pool: pool, KEK: kek, S3: s3, Bucket: bucket, Jobs: jobService, Hub: hub, NATS: nc}
	jobService.OnEnd = orchestrator.JobEnded
	hub.OnExit = orchestrator.SandboxExited
	go jobService.RunReaper(ctx, 5*time.Second)
	go orchestrator.Run(ctx)

	var web fs.FS
	if *webDir != "" {
		web = os.DirFS(*webDir)
	}
	handler := server.New(server.Config{
		Pool:      pool,
		KEK:       kek,
		PublicURL: strings.TrimRight(config.Env("STACKWRIGHT_PUBLIC_URL", "http://127.0.0.1:8080"), "/"),
		SetupCode: setupCode,
		Web:       web,
		State:     state.Store{S3: s3, Bucket: bucket, KEK: kek},
		Jobs:      jobService,
		Runs:      orchestrator,
		Builders:  hub,

		BuilderToken: builderToken,
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

	// The builder's stream needs HTTP/2. Inside compose it runs without TLS.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, Protocols: &protocols}
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

// objectStore returns a client for the server and one that signs URLs for
// sandboxes, which may reach S3 at another address.
func objectStore(ctx context.Context) (client, presign *minio.Client, bucket string, err error) {
	access, err := config.Secret("STACKWRIGHT_S3_ACCESS_KEY")
	if err != nil {
		return nil, nil, "", err
	}
	secret, err := config.Secret("STACKWRIGHT_S3_SECRET_KEY")
	if err != nil {
		return nil, nil, "", err
	}
	connect := func(name, raw string) (*minio.Client, error) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s must be a URL such as http://rustfs:9000", name)
		}
		return minio.New(u.Host, &minio.Options{
			Creds:  credentials.NewStaticV4(access, secret, ""),
			Secure: u.Scheme == "https",
			Region: config.Env("STACKWRIGHT_S3_REGION", "us-east-1"),
		})
	}
	endpoint := config.Env("STACKWRIGHT_S3_ENDPOINT", "http://rustfs:9000")
	if client, err = connect("STACKWRIGHT_S3_ENDPOINT", endpoint); err != nil {
		return nil, nil, "", err
	}
	if presign, err = connect("STACKWRIGHT_S3_PUBLIC_URL", config.Env("STACKWRIGHT_S3_PUBLIC_URL", endpoint)); err != nil {
		return nil, nil, "", err
	}
	bucket = config.Env("STACKWRIGHT_S3_BUCKET", "stackwright")
	exists, err := client.BucketExists(ctx, bucket)
	if err == nil && !exists {
		err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("S3 bucket %s: %w", bucket, err)
	}
	return client, presign, bucket, nil
}
