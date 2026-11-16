package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	SessionCookie = "sw_session"
	CSRFCookie    = "sw_csrf"
	CSRFHeader    = "X-Stackwright-Csrf"
	SessionTTL    = 14 * 24 * time.Hour
)

type User struct {
	ID string
	IP string
	// SSOOrg is the org whose identity provider signed this session or CLI
	// token in, if any (adr/0045).
	SSOOrg string
}

type userKey struct{}

func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok && u.ID != ""
}

func RandomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func CreateSession(ctx context.Context, tx pgx.Tx, userID string) (token, csrf string, err error) {
	token, csrf = RandomToken(), RandomToken()
	_, err = tx.Exec(ctx, `INSERT INTO sessions (id_hash, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4)`,
		HashToken(token), userID, csrf, time.Now().Add(SessionTTL))
	return token, csrf, err
}

type Cookies struct {
	Secure bool
}

func (c Cookies) Set(h http.Header, token, csrf string) {
	maxAge := int(SessionTTL / time.Second)
	if token == "" {
		maxAge = -1
	}
	h.Add("Set-Cookie", (&http.Cookie{Name: SessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode}).String())
	h.Add("Set-Cookie", (&http.Cookie{Name: CSRFCookie, Value: csrf, Path: "/", MaxAge: maxAge,
		Secure: c.Secure, SameSite: http.SameSiteLaxMode}).String())
}

func SessionToken(h http.Header) string {
	c, err := (&http.Request{Header: h}).Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// A request whose CSRF header does not match its session counts as signed
// out. A request from another origin is refused.
func Middleware(pool *pgxpool.Pool, publicURL string, next http.Handler) http.Handler {
	origin := originOf(publicURL)
	errs := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != origin {
			_ = errs.Write(w, r, connect.NewError(connect.CodePermissionDenied, errors.New("cross-origin request")))
			return
		}
		u := User{IP: ClientIP(r)}
		// A CLI token in the Authorization header acts as its user; it is
		// not a cookie, so it needs no CSRF header (adr/0044).
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.HasPrefix(bearer, CLITokenPrefix) {
			id, sso, err := LookupCLIToken(r.Context(), pool, bearer)
			switch {
			case errors.Is(err, ErrBadToken):
				_ = errs.Write(w, r, connect.NewError(connect.CodeUnauthenticated, errors.New("the CLI token is unknown, revoked or unused for 30 days; run stackwright login")))
				return
			case err != nil:
				slog.Error("look up CLI token", "err", err)
				_ = errs.Write(w, r, connect.NewError(connect.CodeInternal, errors.New("internal error")))
				return
			}
			u.ID, u.SSOOrg = id, sso
		} else if token := SessionToken(r.Header); token != "" {
			var csrf string
			err := pool.QueryRow(r.Context(),
				`SELECT user_id, csrf_token, coalesce(sso_org_id::text, '') FROM sessions WHERE id_hash = $1 AND expires_at > now()`,
				HashToken(token)).Scan(&u.ID, &csrf, &u.SSOOrg)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				slog.Error("look up session", "err", err)
				_ = errs.Write(w, r, connect.NewError(connect.CodeInternal, errors.New("internal error")))
				return
			case subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrf)) != 1:
				u.ID, u.SSOOrg = "", ""
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// NavigationUser returns the user of a top-level GET that another site
// sent the browser to, which carries the session cookie but no CSRF
// header. Only a handler that also checks a one-time state bound to this
// user may trust it.
func NavigationUser(ctx context.Context, pool *pgxpool.Pool, r *http.Request) (string, bool) {
	token := SessionToken(r.Header)
	if r.Method != http.MethodGet || token == "" {
		return "", false
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id_hash = $1 AND expires_at > now()`, HashToken(token)).Scan(&id); err != nil {
		return "", false
	}
	return id, true
}

func originOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// safeNext returns next if it is a path on this site, else fallback. A
// browser reads /\host as //host, and drops tabs and newlines from a URL.
func safeNext(next, fallback string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsFunc(next, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return fallback
	}
	return next
}

// ClipName trims a display name to the 100 characters that users.name holds.
func ClipName(name string) string {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 100 {
		return string(r[:100])
	}
	return name
}
