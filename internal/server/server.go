package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/gen/stackwright/v1/stackwrightv1connect"
	"github.com/stackwright/stackwright/internal/api"
	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
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
}

func New(c Config) http.Handler {
	cookies := auth.Cookies{Secure: strings.HasPrefix(c.PublicURL, "https://")}
	signIn := &auth.SignIn{Pool: c.Pool, KEK: c.KEK, PublicURL: c.PublicURL, Cookies: cookies}
	opts := connect.WithInterceptors(&authz.Interceptor{Pool: c.Pool})

	mux := http.NewServeMux()
	serve := func(path string, h http.Handler) {
		mux.Handle(path, auth.Middleware(c.Pool, c.PublicURL, h))
	}
	serve(stackwrightv1connect.NewMetaServiceHandler(api.Meta{}, opts))
	serve(stackwrightv1connect.NewAuthServiceHandler(&api.Auth{Cookies: cookies, SetupCode: c.SetupCode}, opts))
	serve(stackwrightv1connect.NewOrgServiceHandler(&api.Orgs{PublicURL: c.PublicURL}, opts))
	serve(stackwrightv1connect.NewInstanceServiceHandler(&api.Instance{KEK: c.KEK, SignIn: signIn}, opts))
	serve(stackwrightv1connect.NewProjectServiceHandler(api.Projects{}, opts))
	serve(stackwrightv1connect.NewStateServiceHandler(&api.State{Store: c.State, PublicURL: c.PublicURL}, opts))
	serve(stackwrightv1connect.NewTokenServiceHandler(api.Tokens{}, opts))
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
	mux.Handle("/auth/", signIn.Handler())
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		mux.ServeHTTP(w, r)
	})
}
