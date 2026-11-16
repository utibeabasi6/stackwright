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
	"net/netip"
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

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/internal/agent"
	"github.com/utibeabasi6/stackwright/internal/builders"
	"github.com/utibeabasi6/stackwright/internal/config"
	"github.com/utibeabasi6/stackwright/internal/db"
	"github.com/utibeabasi6/stackwright/internal/engine"
	"github.com/utibeabasi6/stackwright/internal/github"
	"github.com/utibeabasi6/stackwright/internal/jobs"
	"github.com/utibeabasi6/stackwright/internal/notify"
	"github.com/utibeabasi6/stackwright/internal/oidc"
	"github.com/utibeabasi6/stackwright/internal/runs"
	"github.com/utibeabasi6/stackwright/internal/server"
	"github.com/utibeabasi6/stackwright/internal/state"
	"github.com/utibeabasi6/stackwright/internal/version"
)

func runServer(args []string) error {
	fl := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fl.String("listen", ":8080", "address to serve on")
	builderListen := fl.String("builder-listen", ":8443", "address to serve builders on, with TLS from the builder CA")
	webDir := fl.String("web", "", "folder with the built web app")
	_ = fl.Parse(args)

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := dbConfig("stackwright_app", "STACKWRIGHT_APP_DB_PASSWORD")
	if err != nil {
		return err
	}
	// No default: a default name would pull from a registry namespace that
	// the project does not hold (adr/0047).
	executorImage := config.Env("STACKWRIGHT_EXECUTOR_IMAGE", "")
	if executorImage == "" {
		return errors.New("set STACKWRIGHT_EXECUTOR_IMAGE to the executor image that build servers run")
	}
	kek, err := loadKEK()
	if err != nil {
		return err
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
	var builderTokens []string
	for _, k := range []string{"STACKWRIGHT_BUILDER_TOKEN", "STACKWRIGHT_BUILDER_TOKEN_PREVIOUS"} {
		t, err := config.Secret(k)
		if err != nil {
			return err
		}
		if t != "" {
			builderTokens = append(builderTokens, t)
		}
	}
	ca, err := builders.LoadCA(ctx, pool, kek)
	if err != nil {
		return err
	}
	binaries, err := builders.LoadBinaries(config.Env("STACKWRIGHT_DOWNLOADS", "/downloads"))
	if err != nil {
		return err
	}
	publicURL := strings.TrimRight(config.Env("STACKWRIGHT_PUBLIC_URL", "http://127.0.0.1:8080"), "/")
	public, err := url.Parse(publicURL)
	if err != nil || public.Host == "" {
		return fmt.Errorf("STACKWRIGHT_PUBLIC_URL %q is not a URL", publicURL)
	}
	builderURL := strings.TrimRight(config.Env("STACKWRIGHT_BUILDER_URL", "https://"+public.Hostname()+":8443"), "/")
	bu, err := url.Parse(builderURL)
	if err != nil || bu.Scheme != "https" || bu.Host == "" {
		return fmt.Errorf("STACKWRIGHT_BUILDER_URL %q is not https://host:port", builderURL)
	}
	sandboxURL := strings.TrimRight(config.Env("STACKWRIGHT_SANDBOX_URL", publicURL), "/")
	internalURL := strings.TrimRight(config.Env("STACKWRIGHT_INTERNAL_URL", "http://stackwright:8080"), "/")
	store, err := objectStore(ctx, internalURL, sandboxURL)
	if err != nil {
		return err
	}
	s3, bucket := store.client, store.bucket
	hosts := []string{bu.Hostname()}
	if extra := config.Env("STACKWRIGHT_BUILDER_HOSTS", ""); extra != "" {
		hosts = append(hosts, strings.Split(extra, ",")...)
	}

	issuer, err := oidc.Load(ctx, pool, kek, strings.TrimRight(config.Env("STACKWRIGHT_OIDC_ISSUER", publicURL), "/"))
	if err != nil {
		return err
	}

	jobService := &jobs.Service{
		Pool: pool, KEK: kek, S3: s3, Presign: store.presign, PresignRemote: store.presignRemote, Relay: store.relay != nil, Bucket: bucket,
		Engines:     &engine.Mirror{Pool: pool, S3: s3, Bucket: bucket, HTTP: &http.Client{Timeout: 10 * time.Minute}},
		Issuer:      issuer,
		STSEndpoint: config.Env("STACKWRIGHT_AWS_STS_ENDPOINT", "https://sts.amazonaws.com"),
	}
	hub := &builders.Hub{
		Pool: pool, CA: ca, BuiltInTokens: builderTokens, BuilderURL: builderURL, Version: version.Version,
		Images:           map[string]string{"docker": executorImage},
		ServerURL:        sandboxURL,
		BuiltInServerURL: internalURL,
	}
	orchestrator := &runs.Orchestrator{Pool: pool, KEK: kek, S3: s3, Bucket: bucket, Jobs: jobService, Hub: hub, NATS: nc}
	jobService.OnEnd = orchestrator.JobEnded
	agentService := &agent.Service{Pool: pool, KEK: kek, Hub: hub, IdleAfter: 30 * time.Minute,
		Model: agent.OpenRouter{URL: strings.TrimRight(config.Env("STACKWRIGHT_OPENROUTER_URL", "https://openrouter.ai/api/v1"), "/"),
			HTTP: &http.Client{Timeout: 10 * time.Minute}}}
	hub.OnExit = func(jobID string, st *v1.SandboxStatus) {
		if agent.IsSandbox(jobID) {
			agentService.SandboxExited(jobID, st)
			return
		}
		orchestrator.SandboxExited(jobID, st)
	}
	gh := &github.GitHub{
		Pool: pool, KEK: kek, PublicURL: publicURL, Runs: orchestrator, S3: s3, Bucket: bucket,
		WebURL: strings.TrimRight(config.Env("STACKWRIGHT_GITHUB_URL", "https://github.com"), "/"),
		APIURL: strings.TrimRight(config.Env("STACKWRIGHT_GITHUB_API_URL", "https://api.github.com"), "/"),
		HTTP:   &http.Client{Timeout: 2 * time.Minute},
	}
	orchestrator.Head = gh.BranchHead
	go gh.RunWebhooks(ctx, 2*time.Second)
	go gh.RunChecks(ctx, 2*time.Second)
	go jobService.RunReaper(ctx, 5*time.Second)
	go jobService.RunRetention(ctx)
	go orchestrator.Run(ctx)
	go orchestrator.Nightly(ctx)
	go orchestrator.Drift(ctx)
	if err := agentService.Recover(ctx); err != nil {
		return err
	}
	go agentService.RunReaper(ctx, time.Minute)
	stsEndpoint := config.Env("STACKWRIGHT_AWS_STS_ENDPOINT", "https://sts.amazonaws.com")
	go (&notify.Sender{Pool: pool, KEK: kek, Client: &http.Client{}, PublicURL: publicURL}).Run(ctx)
	go (&notify.Auditor{Pool: pool, KEK: kek, Issuer: issuer, STSEndpoint: stsEndpoint, Client: &http.Client{},
		S3Endpoint: config.Env("STACKWRIGHT_AUDIT_S3_ENDPOINT", "")}).Run(ctx)

	var web fs.FS
	if *webDir != "" {
		web = os.DirFS(*webDir)
	}
	var trusted []netip.Prefix
	for _, t := range strings.Split(config.Env("STACKWRIGHT_TRUSTED_PROXIES", ""), ",") {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if !strings.Contains(t, "/") {
			ip, err := netip.ParseAddr(t)
			if err != nil {
				return fmt.Errorf("STACKWRIGHT_TRUSTED_PROXIES: %q is not an address or a CIDR", t)
			}
			t = netip.PrefixFrom(ip, ip.BitLen()).String()
		}
		p, err := netip.ParsePrefix(t)
		if err != nil {
			return fmt.Errorf("STACKWRIGHT_TRUSTED_PROXIES: %q is not an address or a CIDR", t)
		}
		trusted = append(trusted, p)
	}
	srvCfg := server.Config{
		Pool:      pool,
		KEK:       kek,
		PublicURL: publicURL,
		SetupCode: setupCode,
		CLIDir:    config.Env("STACKWRIGHT_CLI_DIR", "/cli"),
		// For development only: lets admins point webhooks and SAML
		// metadata at http:// URLs.
		AllowHTTPTargets: config.Env("STACKWRIGHT_ALLOW_HTTP_TARGETS", "") == "1",
		S3Relay:          store.relay,
		TrustedProxies:   trusted,
		Bucket:           bucket,
		Web:              web,
		State:            state.Store{S3: s3, Bucket: bucket, KEK: kek},
		Jobs:             jobService,
		Runs:             orchestrator,
		Builders:         hub,
		Binaries:         binaries,
		Issuer:           issuer,
		GitHub:           gh,
		Agent:            agentService,
	}
	handler := server.New(srvCfg)

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
	builderSrv := &http.Server{Addr: *builderListen, Handler: server.NewBuilderPort(srvCfg), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: ca.TLSConfig(hosts)}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		_ = builderSrv.Shutdown(shutdown)
	}()
	builderErr := make(chan error, 1)
	go func() { builderErr <- builderSrv.ListenAndServeTLS("", "") }()
	slog.Info("server started", "listen", *listen, "builder_listen", *builderListen, "builder_url", builderURL,
		"builder_ca_sha256", ca.Fingerprint(), "version", version.Version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err := <-builderErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type store struct {
	client, presign, presignRemote *minio.Client
	bucket                         string
	// relay is S3 as the server reaches it, when sandboxes reach it only
	// through the server's /s3/ (adr/0046).
	relay *url.URL
}

// objectStore returns a client for the server, and the clients that sign
// URLs for the built-in builder's sandboxes and for remote ones. With
// STACKWRIGHT_S3_PUBLIC_URL set, sandboxes reach S3 there; without it,
// they reach it through the server, at the URL they call it by.
func objectStore(ctx context.Context, internalURL, sandboxURL string) (store, error) {
	var st store
	access, err := config.Secret("STACKWRIGHT_S3_ACCESS_KEY")
	if err != nil {
		return st, err
	}
	secret, err := config.Secret("STACKWRIGHT_S3_SECRET_KEY")
	if err != nil {
		return st, err
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
	if st.client, err = connect("STACKWRIGHT_S3_ENDPOINT", endpoint); err != nil {
		return st, err
	}
	public := config.Env("STACKWRIGHT_S3_PUBLIC_URL", "")
	if public == "" {
		if st.relay, err = url.Parse(endpoint); err != nil {
			return st, err
		}
		if st.presign, err = connect("STACKWRIGHT_INTERNAL_URL", internalURL); err != nil {
			return st, err
		}
		if st.presignRemote, err = connect("STACKWRIGHT_SANDBOX_URL", sandboxURL); err != nil {
			return st, err
		}
	} else {
		// Built-in sandboxes reach S3 through the egress proxy too, so they
		// get the address that every sandbox can reach.
		if st.presignRemote, err = connect("STACKWRIGHT_S3_PUBLIC_URL", public); err != nil {
			return st, err
		}
		st.presign = st.presignRemote
	}
	st.bucket = config.Env("STACKWRIGHT_S3_BUCKET", "stackwright")
	exists, err := st.client.BucketExists(ctx, st.bucket)
	if err == nil && !exists {
		err = st.client.MakeBucket(ctx, st.bucket, minio.MakeBucketOptions{})
	}
	if err != nil {
		return st, fmt.Errorf("S3 bucket %s: %w", st.bucket, err)
	}
	return st, nil
}
