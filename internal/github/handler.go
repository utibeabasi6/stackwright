package github

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stackwright/stackwright/internal/auth"
)

// Callbacks serves the pages GitHub sends the browser back to. Each holds a
// one-time state bound to the signed-in user, which stands in for the
// CSRF header that a navigation from GitHub cannot carry.
func (g *GitHub) Callbacks() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /github/manifest", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.NavigationUser(r.Context(), g.Pool, r)
		if !ok {
			fail(w, r, "", ErrSignedOut)
			return
		}
		to, err := g.FinishManifest(r.Context(), userID, r.URL.Query().Get("state"), r.URL.Query().Get("code"))
		back(w, r, to, err)
	})
	mux.HandleFunc("GET /github/setup", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.NavigationUser(r.Context(), g.Pool, r)
		if !ok {
			fail(w, r, "", ErrSignedOut)
			return
		}
		q := r.URL.Query()
		installation, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
		if err != nil || q.Get("state") == "" {
			fail(w, r, "", errors.New("open Settings › GitHub in Stackwright and start the install from there"))
			return
		}
		next, err := g.AfterInstall(r.Context(), userID, q.Get("state"), installation)
		if err != nil {
			fail(w, r, "", err)
			return
		}
		http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // the configured GitHub URL
	})
	mux.HandleFunc("GET /github/authorize", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.NavigationUser(r.Context(), g.Pool, r)
		if !ok {
			fail(w, r, "", ErrSignedOut)
			return
		}
		to, err := g.Authorize(r.Context(), userID, r.URL.Query().Get("state"), r.URL.Query().Get("code"))
		back(w, r, to, err)
	})
	return mux
}

func back(w http.ResponseWriter, r *http.Request, to string, err error) {
	if err != nil {
		fail(w, r, to, err)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther) //nolint:gosec // a path on this site, checked when the state was made
}

// fail goes back to the page that started the trip with the error, or
// shows it when that page is unknown.
func fail(w http.ResponseWriter, r *http.Request, to string, err error) {
	slog.Warn("GitHub callback", "path", r.URL.Path, "err", err)
	if to == "" {
		http.Error(w, "Stackwright could not finish with GitHub: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, to+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther) //nolint:gosec // a path on this site, checked when the state was made
}
