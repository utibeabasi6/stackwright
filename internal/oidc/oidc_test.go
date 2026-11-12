package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// The example key of RFC 7638, section 3.1.
func TestThumbprintMatchesRFC7638(t *testing.T) {
	n, _ := base64.RawURLEncoding.DecodeString("0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw")
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
	if got := thumbprint(pub); got != "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs" {
		t.Errorf("thumbprint = %s", got)
	}
}

func TestMintedTokenVerifiesAgainstTheJWKS(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{URL: "https://stackwright.example", keys: []key{{kid: thumbprint(&priv.PublicKey), priv: priv}}}
	token, err := is.Mint(Claims{OrgID: "o1", WorkspaceID: "w1", Phase: "plan", RunID: "r1"}, AWSAudience)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	var header struct{ Alg, Kid string }
	var claims struct {
		Iss, Sub, Aud string
		Exp           int64
	}
	decode := func(i int, v any) {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, v) != nil {
			t.Fatalf("part %d: %v", i, err)
		}
	}
	decode(0, &header)
	decode(1, &claims)
	var jwks struct {
		Keys []struct{ Kid, N, E string }
	}
	if err := json.Unmarshal(is.JWKS(), &jwks); err != nil || len(jwks.Keys) != 1 || jwks.Keys[0].Kid != header.Kid || header.Alg != "RS256" {
		t.Fatalf("jwks %s, header %+v", is.JWKS(), header)
	}
	n, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature: %v", err)
	}
	if claims.Iss != is.URL || claims.Aud != AWSAudience || claims.Sub != "org:o1:workspace:w1:phase:plan" {
		t.Errorf("claims: %+v", claims)
	}
	if left := time.Until(time.Unix(claims.Exp, 0)); left > TokenLifetime || left < TokenLifetime-time.Minute {
		t.Errorf("the token lasts %v", left)
	}

	is.URL = "http://127.0.0.1:8080"
	if _, err := is.Mint(Claims{}, AWSAudience); !errors.Is(err, ErrIssuer) {
		t.Errorf("an http issuer: %v", err)
	}
}
