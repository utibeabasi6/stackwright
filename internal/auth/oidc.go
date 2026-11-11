package auth

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"github.com/stackwright/stackwright/internal/seal"
)

var (
	errEmailNotVerified = errors.New("the provider has no verified email for this account")
	errNoAccount        = errors.New("no account for this email")
)

const loginCookie = "sw_login"

type identity struct {
	Subject string
	Email   string
	Name    string
}

type provider struct {
	cfg      oauth2.Config
	verifier *oidc.IDTokenVerifier
	github   bool
}

type SignIn struct {
	Pool      *pgxpool.Pool
	KEK       seal.KEK
	PublicURL string
	Cookies   Cookies

	mu    sync.Mutex
	cache map[string]*provider
}

func (s *SignIn) CallbackURL(providerID string) string {
	return s.PublicURL + "/auth/oidc/" + providerID + "/callback"
}

func (s *SignIn) Forget(providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, providerID)
}

func (s *SignIn) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/oidc/{id}/start", s.start)
	mux.HandleFunc("GET /auth/oidc/{id}/callback", s.callback)
	return mux
}

func (s *SignIn) provider(ctx context.Context, id string) (*provider, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, pgx.ErrNoRows
	}
	s.mu.Lock()
	p := s.cache[id]
	s.mu.Unlock()
	if p != nil {
		return p, nil
	}

	var kind, issuer, clientID string
	var secret []byte
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT kind, issuer, client_id, client_secret FROM sign_in_providers WHERE id = $1`,
			id).Scan(&kind, &issuer, &clientID, &secret); err != nil {
			return err
		}
		var err error
		secret, err = s.KEK.Open(ctx, tx, "", secret)
		return err
	})
	if err != nil {
		return nil, err
	}
	p = &provider{cfg: oauth2.Config{ClientID: clientID, ClientSecret: string(secret), RedirectURL: s.CallbackURL(id)}}
	if kind == "github" {
		p.github = true
		p.cfg.Endpoint = github.Endpoint
		p.cfg.Scopes = []string{"read:user", "user:email"}
	} else {
		op, err := oidc.NewProvider(ctx, issuer)
		if err != nil {
			return nil, fmt.Errorf("discover %s: %w", issuer, err)
		}
		p.cfg.Endpoint = op.Endpoint()
		p.cfg.Scopes = []string{oidc.ScopeOpenID, "email", "profile"}
		p.verifier = op.Verifier(&oidc.Config{ClientID: clientID})
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]*provider{}
	}
	s.cache[id] = p
	s.mu.Unlock()
	return p, nil
}

type loginState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Invite   string `json:"i,omitempty"`
}

func (s *SignIn) start(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.provider(r.Context(), id)
	if err != nil {
		s.fail(w, r, "provider", err)
		return
	}
	st := loginState{Provider: id, State: RandomToken(), Nonce: RandomToken(), Verifier: oauth2.GenerateVerifier(),
		Invite: r.URL.Query().Get("invite")}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: base64.RawURLEncoding.EncodeToString(raw), Path: "/auth/oidc/",
		MaxAge: 600, HttpOnly: true, Secure: s.Cookies.Secure, SameSite: http.SameSiteLaxMode})
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier)}
	if !p.github {
		opts = append(opts, oidc.Nonce(st.Nonce))
	}
	http.Redirect(w, r, p.cfg.AuthCodeURL(st.State, opts...), http.StatusFound)
}

func (s *SignIn) fail(w http.ResponseWriter, r *http.Request, reason string, err error) {
	slog.Warn("sign-in failed", "reason", reason, "err", err)
	http.Redirect(w, r, "/sign-in?error="+url.QueryEscape(reason), http.StatusFound)
}

func (s *SignIn) callback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/auth/oidc/", MaxAge: -1, HttpOnly: true,
		Secure: s.Cookies.Secure, SameSite: http.SameSiteLaxMode})
	id := r.PathValue("id")
	var st loginState
	c, err := r.Cookie(loginCookie)
	if err == nil {
		var raw []byte
		if raw, err = base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			err = json.Unmarshal(raw, &st)
		}
	}
	q := r.URL.Query()
	switch {
	case err != nil || st.Provider != id:
		s.fail(w, r, "expired", err)
		return
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1:
		s.fail(w, r, "state", nil)
		return
	case q.Get("error") != "":
		s.fail(w, r, "denied", errors.New(q.Get("error")))
		return
	}
	p, err := s.provider(r.Context(), id)
	if err != nil {
		s.fail(w, r, "provider", err)
		return
	}
	ident, err := p.identity(r.Context(), q.Get("code"), st.Verifier, st.Nonce)
	if errors.Is(err, errEmailNotVerified) {
		s.fail(w, r, "email", err)
		return
	}
	if err != nil {
		s.fail(w, r, "provider", err)
		return
	}

	next := "/"
	var token, csrf string
	err = pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		userID, err := s.account(r.Context(), tx, id, ident, st.Invite, &next)
		if err != nil {
			return err
		}
		if token, csrf, err = CreateSession(r.Context(), tx, userID); err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_log (actor_kind, actor_id, action, target, ip)
			VALUES ('user', $1, 'auth.sign_in', $2, nullif($3, '')::inet)`, userID, "provider:"+id, clientIP(r))
		return err
	})
	switch {
	case errors.Is(err, errNoAccount):
		s.fail(w, r, "no-account", err)
		return
	case errors.Is(err, ErrInviteNotFound), errors.Is(err, ErrInviteEmail):
		s.fail(w, r, "invite", err)
		return
	case err != nil:
		s.fail(w, r, "internal", err)
		return
	}
	s.Cookies.Set(w.Header(), token, csrf)
	http.Redirect(w, r, next, http.StatusFound)
}

// The rules are in docs/adr/0013-sign-in-providers-and-accounts.md.
func (s *SignIn) account(ctx context.Context, tx pgx.Tx, providerID string, id identity, inviteToken string, next *string) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider_id = $1 AND subject = $2`,
		providerID, id.Subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, id.Email).Scan(&userID)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows) && inviteToken == "":
		return "", errNoAccount
	case errors.Is(err, pgx.ErrNoRows):
		userID = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`,
			userID, id.Email, id.Name); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identities (provider_id, subject, user_id) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, providerID, id.Subject, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.user_id', $1, true)`, userID); err != nil {
		return "", err
	}
	if inviteToken != "" {
		inv, err := LookupInvite(ctx, tx, inviteToken)
		if err != nil {
			return "", err
		}
		if err := AcceptInvite(ctx, tx, inv, userID, id.Email); err != nil {
			return "", err
		}
		*next = "/" + inv.OrgSlug
	}
	return userID, nil
}

func (p *provider) identity(ctx context.Context, code, verifier, nonce string) (identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := p.cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity{}, fmt.Errorf("exchange code: %w", err)
	}
	if p.github {
		return githubIdentity(ctx, p.cfg.Client(ctx, tok))
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return identity{}, errors.New("token response has no id_token")
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return identity{}, fmt.Errorf("verify id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return identity{}, errors.New("id_token nonce does not match")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil {
		return identity{}, err
	}
	if claims.Email == "" || !claims.EmailVerified {
		return identity{}, errEmailNotVerified
	}
	return identity{Subject: idt.Subject, Email: claims.Email, Name: claims.Name}, nil
}

func githubIdentity(ctx context.Context, client *http.Client) (identity, error) {
	get := func(path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := get("/user", &user); err != nil {
		return identity{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := get("/user/emails", &emails); err != nil {
		return identity{}, err
	}
	id := identity{Subject: strconv.FormatInt(user.ID, 10), Name: cmp.Or(user.Name, user.Login)}
	for _, e := range emails {
		if e.Primary && e.Verified {
			id.Email = e.Email
		}
	}
	if id.Email == "" {
		return identity{}, errEmailNotVerified
	}
	return id, nil
}
