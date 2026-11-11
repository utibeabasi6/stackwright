package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stackwright/stackwright/internal/server"
	"github.com/stackwright/stackwright/internal/testenv"
)

func get(h http.Handler, method, path string) (int, http.Header, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code, rec.Header(), rec.Body.String()
}

func TestServer(t *testing.T) {
	web := fstest.MapFS{
		"index.html":    {Data: []byte("<div id=root>")},
		"assets/app.js": {Data: []byte("app")},
	}
	h := server.New(testenv.Postgres(t), web)

	code, header, body := get(h, http.MethodGet, "/healthz")
	if code != http.StatusOK || body != "ok\n" {
		t.Errorf("healthz = %d %q", code, body)
	}
	if !strings.Contains(header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("missing CSP: %q", header.Get("Content-Security-Policy"))
	}
	if _, _, body := get(h, http.MethodGet, "/assets/app.js"); body != "app" {
		t.Errorf("asset = %q", body)
	}
	if _, _, body := get(h, http.MethodGet, "/build-servers"); !strings.Contains(body, "root") {
		t.Errorf("deep link = %q, want index.html", body)
	}
	if code, _, _ := get(h, http.MethodPost, "/build-servers"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST to the app = %d", code)
	}
}
