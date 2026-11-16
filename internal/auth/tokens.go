package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBadToken = errors.New("the token is unknown, expired or revoked")

const tokenPrefix = "swt_"

func NewAPIToken() string {
	return tokenPrefix + RandomToken()
}

func LookupToken(ctx context.Context, pool *pgxpool.Pool, token string) (userID, orgID string, err error) {
	var stale bool
	err = pool.QueryRow(ctx, `SELECT user_id, org_id, last_used_at IS NULL OR last_used_at < now() - interval '1 minute'
		FROM api_tokens WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`,
		HashToken(token)).Scan(&userID, &orgID, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrBadToken
	}
	if err != nil {
		return "", "", err
	}
	if stale {
		_, _ = pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE token_hash = $1`, HashToken(token), time.Now())
	}
	return userID, orgID, nil
}

const (
	JobTokenPrefix = "swj_"
	// A state token reaches only its job's state, so the engine, and the
	// code it runs, never holds the job's token (adr/0047).
	StateTokenPrefix = "swe_"
)

func NewJobToken() string {
	return JobTokenPrefix + RandomToken()
}
