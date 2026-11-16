package server

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/agent"
	"github.com/stackwright/stackwright/internal/api"
	"github.com/stackwright/stackwright/internal/artifact"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
	"github.com/stackwright/stackwright/internal/builders"
	"github.com/stackwright/stackwright/internal/github"
	"github.com/stackwright/stackwright/internal/jobs"
	"github.com/stackwright/stackwright/internal/oidc"
	"github.com/stackwright/stackwright/internal/runs"
	"github.com/stackwright/stackwright/internal/scim"
	"github.com/stackwright/stackwright/internal/seal"
	"github.com/stackwright/stackwright/internal/state"
)

type Config struct {
	Pool      *pgxpool.Pool
	KEK       seal.KEK
	PublicURL string
	SetupCode string
	Web       fs.FS
	State     state.Store
	Jobs      *jobs.Service
	Runs      *runs.Orchestrator
	Builders  *builders.Hub
	// Binaries are the builder binaries that the builder port hands out.
	Binaries map[string]builders.Binary
	Issuer   *oidc.Issuer
	GitHub   *github.GitHub
	Agent    *agent.Service
	// CLIDir holds the gzipped CLI binaries and their SHA256SUMS (adr/0044).
	CLIDir string
	// AllowHTTPTargets lets webhooks and SAML metadata use http://.
	AllowHTTPTargets bool
	// S3Relay is S3 as the server reaches it, when sandboxes reach it only
	// through /s3/; Bucket is the one bucket the relay serves (adr/0046).
	S3Relay *url.URL
	Bucket  string
	// TrustedProxies may say who the client is, in X-Forwarded-For.
	TrustedProxies []netip.Prefix
}

var cliBinary = regexp.MustCompile(`^stackwright-(darwin|linux)-(arm64|amd64)$`)

func New(c Config) http.Handler {
	cookies := auth.Cookies{Secure: strings.HasPrefix(c.PublicURL, "https://")}
	signIn := &auth.SignIn{Pool: c.Pool, KEK: c.KEK, PublicURL: c.PublicURL, Cookies: cookies}
	// Connect reads, and inflates, a whole request before any check runs,
	// so every service has a size limit; a few raise it.
	opts := connect.WithOptions(connect.WithInterceptors(&authz.Interceptor{Pool: c.Pool, Limit: userLimit()}),
		connect.WithReadMaxBytes(4<<20))

	mux := http.NewServeMux()
	serve := func(path string, h http.Handler) {
		mux.Handle(path, auth.Middleware(c.Pool, c.PublicURL, h))
	}
	serve(stackwrightv1connect.NewMetaServiceHandler(api.Meta{}, opts))
	serve(stackwrightv1connect.NewAuthServiceHandler(&api.Auth{Cookies: cookies, SetupCode: c.SetupCode}, opts))
	serve(stackwrightv1connect.NewOrgServiceHandler(&api.Orgs{PublicURL: c.PublicURL}, opts))
	serve(stackwrightv1connect.NewInstanceServiceHandler(&api.Instance{KEK: c.KEK, SignIn: signIn}, opts))
	serve(stackwrightv1connect.NewProjectServiceHandler(api.Projects{Engines: c.Jobs.Engines}, opts))
	serve(stackwrightv1connect.NewStateServiceHandler(&api.State{Store: c.State, PublicURL: c.PublicURL}, opts))
	serve(stackwrightv1connect.NewTokenServiceHandler(api.Tokens{}, opts))
	serve(stackwrightv1connect.NewExecutorServiceHandler(api.Executor{Jobs: c.Jobs, Changed: c.Runs.Changed}, opts))
	serve(stackwrightv1connect.NewRunServiceHandler(api.Runs{Pool: c.Pool, Orchestrator: c.Runs, GitHub: c.GitHub}, opts, connect.WithReadMaxBytes(48<<20)))
	serve(stackwrightv1connect.NewImportServiceHandler(api.Imports{GitHub: c.GitHub, Orchestrator: c.Runs}, opts))
	serve(stackwrightv1connect.NewSecurityServiceHandler(api.Security{Jobs: c.Jobs, Orchestrator: c.Runs}, opts))
	serve(stackwrightv1connect.NewDeploymentServiceHandler(api.Deployments{Orchestrator: c.Runs}, opts))
	serve(stackwrightv1connect.NewSsoServiceHandler(api.SSO{PublicURL: c.PublicURL, AllowHTTP: c.AllowHTTPTargets}, opts))
	serve(stackwrightv1connect.NewNotificationServiceHandler(api.Notifications{KEK: c.KEK, AllowHTTP: c.AllowHTTPTargets}, opts))
	serve(stackwrightv1connect.NewBuilderServiceHandler(c.Builders, opts))
	serve(stackwrightv1connect.NewBuildServerServiceHandler(api.BuildServers{KEK: c.KEK, Hub: c.Builders, Binaries: c.Binaries}, opts))
	serve(stackwrightv1connect.NewVariableServiceHandler(api.Variables{KEK: c.KEK}, opts))
	serve(stackwrightv1connect.NewCloudAccountServiceHandler(api.CloudAccounts{Issuer: c.Issuer}, opts))
	serve(stackwrightv1connect.NewGitHubServiceHandler(api.GitHub{GitHub: c.GitHub}, opts))
	serve(stackwrightv1connect.NewGraphServiceHandler(api.Graph{Jobs: c.Jobs, GitHubURL: c.GitHub.WebURL}, opts))
	hooks := agentHooks{pool: c.Pool, jobs: c.Jobs, runs: c.Runs, github: c.GitHub}
	c.Agent.Hooks = agent.Hooks{EngineDownload: c.Jobs.EngineDownload, Plan: hooks.plan, PlanDetails: hooks.planDetails,
		StateLookup: hooks.stateLookup, OpenPullRequest: hooks.openPullRequest,
		QueuePlan: func(ctx context.Context, orgID, session, userID string, source []byte) (int32, error) {
			_, number, err := hooks.queuePlan(ctx, orgID, session, userID, source)
			return number, err
		}}
	serve(stackwrightv1connect.NewAgentServiceHandler(c.Agent, opts, connect.WithReadMaxBytes(16<<20)))
	serve(stackwrightv1connect.NewToolboxServiceHandler(c.Agent, opts, connect.WithReadMaxBytes(40<<20)))
	mux.Handle("/git/", c.GitHub.GitProxy())
	mux.Handle("/lsp/", c.Agent.LanguageServer())
	mux.Handle("/github/", c.GitHub.Callbacks())
	mux.HandleFunc("POST /webhooks/github", c.GitHub.Webhook)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c.Issuer.Discovery())
	})
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c.Issuer.JWKS())
	})
	mux.Handle("/state/", state.Handler{Pool: c.Pool, Store: c.State})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := c.Pool.Ping(ctx); err != nil {
			http.Error(w, "postgres unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	if c.S3Relay != nil {
		mux.Handle("/s3/", s3Relay(c.S3Relay, c.Bucket))
	}
	mux.Handle("/scim/v2/", (&scim.Server{Pool: c.Pool, PublicURL: c.PublicURL}).Handler())
	mux.Handle("/auth/saml/", (&auth.SAML{Pool: c.Pool, PublicURL: c.PublicURL, Cookies: cookies}).Handler())
	mux.Handle("/auth/", signIn.Handler())
	mux.Handle("/cli/device/", auth.Device{Pool: c.Pool, PublicURL: c.PublicURL}.Handler())
	mux.HandleFunc("GET /cli/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "SHA256SUMS" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.ServeFile(w, r, filepath.Join(c.CLIDir, "SHA256SUMS"))
			return
		}
		if !cliBinary.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(c.CLIDir, name+".gz")) //nolint:gosec // a name the pattern allows, in the CLI folder
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="stackwright"`)
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.Copy(w, f)
			return
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			http.Error(w, "the CLI file is damaged", http.StatusInternalServerError)
			return
		}
		_, _ = io.Copy(w, zr) //nolint:gosec // a file the image ships, not user input
	})
	if c.Web != nil {
		files := http.FileServerFS(c.Web)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			name := path.Clean(r.URL.Path)[1:]
			if name == "" {
				name = "index.html"
			}
			if _, err := fs.Stat(c.Web, name); errors.Is(err, os.ErrNotExist) {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
			files.ServeHTTP(w, r)
		})
	}
	return trustProxies(c.TrustedProxies, rateLimit(secure(mux, c.GitHub.WebURL)))
}

// secure sets the headers of every page. formTargets are the origins a
// form may post to besides this one: GitHub takes the App manifest as a
// form.
func secure(next http.Handler, formTargets ...string) http.Handler {
	csp := "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action " +
		strings.Join(append([]string{"'self'"}, formTargets...), " ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// NewBuilderPort serves builders on the port whose TLS the builder CA
// signs (adr/0021): Enrol, Connect with a client certificate, and the
// builder binaries.
func NewBuilderPort(c Config) http.Handler {
	opts := connect.WithOptions(connect.WithInterceptors(&authz.Interceptor{Pool: c.Pool}), connect.WithReadMaxBytes(4<<20))
	mux := http.NewServeMux()
	path, h := stackwrightv1connect.NewBuilderServiceHandler(c.Builders, opts)
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			r = r.WithContext(authz.WithPeerCertificate(r.Context(), r.TLS.VerifiedChains[0][0]))
		}
		h.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /downloads/{name}", func(w http.ResponseWriter, r *http.Request) {
		platform, ok := strings.CutPrefix(r.PathValue("name"), "stackwright-builder-")
		bin, found := c.Binaries[platform]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, bin.Path)
	})
	return secure(mux)
}

// s3Relay forwards a presigned GET, HEAD or PUT on the bucket to S3, with
// the Host header that the URL was signed for (adr/0046).
func s3Relay(target *url.URL, bucket string) http.Handler {
	rp := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
		pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/s3")
		pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.RawPath, "/s3")
		pr.Out.Host = pr.In.Host
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPut:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		case !strings.HasPrefix(r.URL.Path, "/s3/"+bucket+"/") || r.URL.Query().Get("X-Amz-Signature") == "":
			http.Error(w, "only presigned URLs of the bucket", http.StatusForbidden)
		case r.Method == http.MethodPut && (r.ContentLength < 0 || r.ContentLength > artifact.MaxSize):
			http.Error(w, "a PUT needs a Content-Length of at most 128 MiB", http.StatusRequestEntityTooLarge)
		default:
			rp.ServeHTTP(w, r)
		}
	})
}
