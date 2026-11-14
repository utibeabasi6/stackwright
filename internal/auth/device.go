package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CLITokenPrefix starts a token of the CLI (adr/0044).
	CLITokenPrefix = "swc_"
	// CLITokenIdle ends a CLI token that has not been used for this long.
	CLITokenIdle   = 30 * 24 * time.Hour
	deviceLifetime = 10 * time.Minute
	pollInterval   = 5 * time.Second
	// Consonants only, so a code spells no word; no vowels, no lookalikes.
	userCodeLetters = "BCDFGHJKLMNPQRSTVWXZ"
)

// UserCode formats a code the way the device page asks for it: eight
// letters, with a hyphen in the middle, in upper case.
func UserCode(s string) string {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(s))
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:]
}

// LookupCLIToken returns the user of a CLI token that is not revoked and
// was used in the last CLITokenIdle.
func LookupCLIToken(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	var userID string
	var stale bool
	err := pool.QueryRow(ctx, `SELECT user_id, last_used_at < now() - interval '1 minute' FROM cli_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND last_used_at > $2`, HashToken(token), time.Now().Add(-CLITokenIdle)).Scan(&userID, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	if stale {
		_, _ = pool.Exec(ctx, `UPDATE cli_tokens SET last_used_at = now() WHERE token_hash = $1`, HashToken(token))
	}
	return userID, nil
}

// Device serves the two endpoints of the OAuth device flow that the CLI
// calls, as RFC 8628 names them; a signed-in user approves the code in the
// web app.
// https://www.rfc-editor.org/rfc/rfc8628
type Device struct {
	Pool      *pgxpool.Pool
	PublicURL string
}

func (d Device) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cli/device/code", d.code)
	mux.HandleFunc("POST /cli/device/token", d.token)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// https://www.rfc-editor.org/rfc/rfc8628#section-3.2
func (d Device) code(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "name the machine, in 1 to 100 characters"})
		return
	}
	device := RandomToken()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = userCodeLetters[int(b[i])%len(userCodeLetters)]
	}
	user := UserCode(string(b))
	ctx := r.Context()
	if _, err := d.Pool.Exec(ctx, `DELETE FROM device_codes WHERE expires_at < now()`); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO device_codes (device_hash, user_code, name, expires_at) VALUES ($1, $2, $3, $4)`,
		HashToken(device), user, name, time.Now().Add(deviceLifetime)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	verify := strings.TrimSuffix(d.PublicURL, "/") + "/device"
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": device, "user_code": user, "verification_uri": verify, "verification_uri_complete": verify + "?code=" + user,
		"expires_in": int(deviceLifetime / time.Second), "interval": int(pollInterval / time.Second),
	})
}

// https://www.rfc-editor.org/rfc/rfc8628#section-3.4
func (d Device) token(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	ctx := r.Context()
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hash := HashToken(r.FormValue("device_code"))
	var name string
	var expired, denied, early bool
	var approvedBy *string
	err = tx.QueryRow(ctx, `SELECT name, expires_at < now(), denied, approved_by::text, coalesce(polled_at > $2, false) FROM device_codes
		WHERE device_hash = $1 FOR UPDATE`, hash, time.Now().Add(-pollInterval)).Scan(&name, &expired, &denied, &approvedBy, &early)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expired_token"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	reply := func(status int, v map[string]string, done bool) {
		q := `UPDATE device_codes SET polled_at = now() WHERE device_hash = $1`
		if done {
			q = `DELETE FROM device_codes WHERE device_hash = $1`
		}
		if _, err := tx.Exec(ctx, q, hash); err != nil || tx.Commit(ctx) != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		writeJSON(w, status, v)
	}
	switch {
	case expired:
		reply(http.StatusBadRequest, map[string]string{"error": "expired_token"}, true)
	case denied:
		reply(http.StatusBadRequest, map[string]string{"error": "access_denied"}, true)
	case early:
		reply(http.StatusBadRequest, map[string]string{"error": "slow_down"}, false)
	case approvedBy == nil:
		reply(http.StatusBadRequest, map[string]string{"error": "authorization_pending"}, false)
	default:
		token := CLITokenPrefix + RandomToken()
		if _, err := tx.Exec(ctx, `INSERT INTO cli_tokens (id, user_id, token_hash, name) VALUES ($1, $2, $3, $4)`,
			uuid.NewString(), *approvedBy, HashToken(token), name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		reply(http.StatusOK, map[string]string{"access_token": token, "token_type": "Bearer"}, true)
	}
}
