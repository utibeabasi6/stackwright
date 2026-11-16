// Package github connects the instance's GitHub App: its setup, webhooks,
// the runs they start, and the checks and comments that report them
// (adr/0023).
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"

	"github.com/stackwright/stackwright/internal/jws"
	"github.com/stackwright/stackwright/internal/runs"
	"github.com/stackwright/stackwright/internal/seal"
)

var (
	ErrNoApp      = errors.New("this instance has no GitHub App yet")
	ErrNotFound   = errors.New("GitHub answered 404")
	ErrForbidden  = errors.New("GitHub refused the request")
	ErrBadRequest = errors.New("GitHub refused the request as invalid")
)

// GitHub holds the App's connection and everything that acts on its
// events. One server process runs its loops.
type GitHub struct {
	Pool *pgxpool.Pool
	KEK  seal.KEK
	// WebURL is github.com and APIURL api.github.com, or a GitHub
	// Enterprise Server's.
	WebURL    string
	APIURL    string
	PublicURL string
	HTTP      *http.Client
	Runs      *runs.Orchestrator
	S3        *minio.Client
	Bucket    string

	mu     sync.Mutex
	app    *App
	tokens map[int64]token
}

type token struct {
	value   string
	expires time.Time
}

// App is the instance's GitHub App, with its secrets opened.
type App struct {
	ID            int64
	Slug          string
	Name          string
	HTMLURL       string
	Owner         string
	ClientID      string
	ClientSecret  string
	WebhookSecret []byte
	key           *rsa.PrivateKey
}

// App loads the App once and keeps it.
func (g *GitHub) App(ctx context.Context) (*App, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.app != nil {
		return g.app, nil
	}
	a := &App{}
	var clientSecret, webhookSecret, key []byte
	err := pgx.BeginFunc(ctx, g.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT app_id, slug, name, html_url, owner, client_id, client_secret, webhook_secret, private_key FROM github_app`).
			Scan(&a.ID, &a.Slug, &a.Name, &a.HTMLURL, &a.Owner, &a.ClientID, &clientSecret, &webhookSecret, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoApp
		}
		if err != nil {
			return err
		}
		secret, err := g.KEK.Open(ctx, tx, "", clientSecret)
		if err != nil {
			return err
		}
		a.ClientSecret = string(secret)
		if a.WebhookSecret, err = g.KEK.Open(ctx, tx, "", webhookSecret); err != nil {
			return err
		}
		pemKey, err := g.KEK.Open(ctx, tx, "", key)
		if err != nil {
			return err
		}
		a.key, err = parseKey(pemKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	g.app = a
	return a, nil
}

// forget drops the cached App after it changes.
func (g *GitHub) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.app, g.tokens = nil, nil
}

// dropToken forgets a cached installation token that GitHub refused: a
// token keeps the App's permissions from when it was made, so after an org
// accepts new ones only a fresh token has them.
func (g *GitHub) dropToken(value string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, t := range g.tokens {
		if t.value == value {
			delete(g.tokens, id)
		}
	}
}

func parseKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the App's private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the App's private key is not RSA")
	}
	return rsaKey, nil
}

// installationToken returns a token for the installation, and keeps it
// until five minutes before it ends.
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
func (g *GitHub) installationToken(ctx context.Context, installation int64) (string, error) {
	app, err := g.App(ctx)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	t, ok := g.tokens[installation]
	g.mu.Unlock()
	if ok && time.Until(t.expires) > 5*time.Minute {
		return t.value, nil
	}
	now := time.Now()
	jwt, err := jws.SignRS256(app.key, "", map[string]any{
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(app.ID, 10),
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installation), jwt, nil, &out); err != nil {
		return "", fmt.Errorf("installation %d token: %w", installation, err)
	}
	g.mu.Lock()
	if g.tokens == nil {
		g.tokens = map[int64]token{}
	}
	g.tokens[installation] = token{value: out.Token, expires: out.ExpiresAt}
	g.mu.Unlock()
	return out.Token, nil
}

// call sends a REST request to the API. path starts with /.
func (g *GitHub) call(ctx context.Context, method, path, bearer string, in, out any) error {
	resp, err := g.request(ctx, method, g.APIURL+path, bearer, in)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

func (g *GitHub) request(ctx context.Context, method, url, bearer string, in any) (*http.Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body) //nolint:gosec // url is the configured GitHub web or API URL
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req) //nolint:gosec // url is the configured GitHub web or API URL
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		msg := strings.TrimSpace(string(b))
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%w: %s %s", ErrNotFound, method, url)
		case http.StatusUnauthorized, http.StatusForbidden:
			g.dropToken(bearer)
			return nil, fmt.Errorf("%w: %s %s: %s", ErrForbidden, method, url, msg)
		case http.StatusUnprocessableEntity:
			return nil, fmt.Errorf("%w: %s %s: %s", ErrBadRequest, method, url, msg)
		}
		return nil, fmt.Errorf("GitHub %s %s: %s: %s", method, url, resp.Status, msg)
	}
	return resp, nil
}
