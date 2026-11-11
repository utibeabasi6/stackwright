package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/gen/stackwright/meta/v1/metav1connect"
	"github.com/stackwright/stackwright/internal/meta"
)

func New(db *pgxpool.Pool, web fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, "postgres unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle(metav1connect.NewMetaServiceHandler(meta.Service{}))
	if web != nil {
		files := http.FileServerFS(web)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			name := path.Clean(r.URL.Path)[1:]
			if name == "" {
				name = "index.html"
			}
			if _, err := fs.Stat(web, name); errors.Is(err, os.ErrNotExist) {
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
