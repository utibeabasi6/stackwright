// Package jws signs compact JWTs with RS256 (RFC 7515, RFC 7519).
package jws

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

// SignRS256 returns header.claims.signature. kid may be empty.
func SignRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	h := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		h["kid"] = kid
	}
	header, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
