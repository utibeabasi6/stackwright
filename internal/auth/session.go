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
	"time"

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
		if token := SessionToken(r.Header); token != "" {
			var csrf string
			err := pool.QueryRow(r.Context(),
				`SELECT user_id, csrf_token FROM sessions WHERE id_hash = $1 AND expires_at > now()`,
				HashToken(token)).Scan(&u.ID, &csrf)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				slog.Error("look up session", "err", err)
				_ = errs.Write(w, r, connect.NewError(connect.CodeInternal, errors.New("internal error")))
				return
			case subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrf)) != 1:
				u.ID = ""
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
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
