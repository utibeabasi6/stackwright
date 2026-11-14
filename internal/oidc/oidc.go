// Package oidc issues one short-lived JWT for each job, which a cloud
// trades for credentials (adr/0022). AWS fetches the discovery document and
// the keys from the issuer URL on every exchange.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/internal/jws"
	"github.com/stackwright/stackwright/internal/seal"
)

// AWSAudience is the audience AWS STS expects.
const AWSAudience = "sts.amazonaws.com"

// TokenLifetime is short: the executor trades the token as soon as it
// claims its job.
const TokenLifetime = 15 * time.Minute

var ErrIssuer = errors.New("the OIDC issuer must be an https:// URL with no trailing slash")

type key struct {
	kid  string
	priv *rsa.PrivateKey
}

// Issuer signs with the newest key and publishes every key.
type Issuer struct {
	URL  string
	keys []key
}

// Load reads the signing keys, and makes one on first use.
func Load(ctx context.Context, pool *pgxpool.Pool, kek seal.KEK, url string) (*Issuer, error) {
	is := &Issuer{URL: url}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM oidc_keys`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				return err
			}
			der, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				return err
			}
			sealed, err := kek.Seal(ctx, tx, "", der)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO oidc_keys (kid, private_key) VALUES ($1, $2)`, thumbprint(&priv.PublicKey), sealed); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT kid, private_key FROM oidc_keys ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		type stored struct {
			kid    string
			sealed []byte
		}
		all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stored, error) {
			var s stored
			return s, r.Scan(&s.kid, &s.sealed)
		})
		if err != nil {
			return err
		}
		for _, s := range all {
			der, err := kek.Open(ctx, tx, "", s.sealed)
			if err != nil {
				return err
			}
			priv, err := x509.ParsePKCS8PrivateKey(der)
			if err != nil {
				return err
			}
			rsaKey, ok := priv.(*rsa.PrivateKey)
			if !ok {
				return fmt.Errorf("OIDC key %s is not RSA", s.kid)
			}
			is.keys = append(is.keys, key{kid: s.kid, priv: rsaKey})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load the OIDC keys: %w", err)
	}
	return is, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwk(pub *rsa.PublicKey) map[string]string {
	return map[string]string{"kty": "RSA", "n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}
}

// thumbprint is the key's JWK thumbprint (RFC 7638): the SHA-256 of its
// required members, in this order, with no spaces.
func thumbprint(pub *rsa.PublicKey) string {
	k := jwk(pub)
	sum := sha256.Sum256(fmt.Appendf(nil, `{"e":%q,"kty":"RSA","n":%q}`, k["e"], k["n"]))
	return b64(sum[:])
}

// Subject is what trust policies match on. AWS can match only on sub and
// aud, so sub names the workspace and the phase by their ids, which survive
// renames.
func Subject(orgID, workspaceID, phase string) string {
	return fmt.Sprintf("org:%s:workspace:%s:phase:%s", orgID, workspaceID, phase)
}

// Claims are a job's token. Only the ids and the phase make up its subject;
// the rest is for audit.
type Claims struct {
	OrgID, OrgSlug             string
	ProjectID, ProjectSlug     string
	WorkspaceID, WorkspaceName string
	RunID, JobID               string
	Phase                      string
}

// Mint signs a token for aud that ends at TokenLifetime.
func (is *Issuer) Mint(c Claims, aud ...string) (string, error) {
	if !strings.HasPrefix(is.URL, "https://") || strings.HasSuffix(is.URL, "/") {
		return "", ErrIssuer
	}
	var audience any = aud
	if len(aud) == 1 {
		audience = aud[0]
	}
	k := is.keys[0]
	now := time.Now()
	return jws.SignRS256(k.priv, k.kid, map[string]any{
		"iss": is.URL, "sub": Subject(c.OrgID, c.WorkspaceID, c.Phase), "aud": audience,
		"iat": now.Unix(), "nbf": now.Add(-30 * time.Second).Unix(), "exp": now.Add(TokenLifetime).Unix(),
		"org": c.OrgSlug, "org_id": c.OrgID, "project": c.ProjectSlug, "project_id": c.ProjectID,
		"workspace": c.WorkspaceName, "workspace_id": c.WorkspaceID, "run_id": c.RunID, "job_id": c.JobID, "phase": c.Phase,
	})
}

// Discovery is /.well-known/openid-configuration.
func (is *Issuer) Discovery() []byte {
	b, _ := json.MarshalIndent(map[string]any{
		"issuer":                                is.URL,
		"jwks_uri":                              is.URL + "/.well-known/jwks.json",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported": []string{"sub", "aud", "exp", "iat", "iss", "org", "org_id", "project", "project_id",
			"workspace", "workspace_id", "run_id", "job_id", "phase"},
	}, "", "  ")
	return b
}

// JWKS is /.well-known/jwks.json.
func (is *Issuer) JWKS() []byte {
	keys := make([]map[string]string, 0, len(is.keys))
	for _, k := range is.keys {
		j := jwk(&k.priv.PublicKey)
		j["kid"], j["alg"], j["use"] = k.kid, "RS256", "sig"
		keys = append(keys, j)
	}
	b, _ := json.MarshalIndent(map[string]any{"keys": keys}, "", "  ")
	return b
}
