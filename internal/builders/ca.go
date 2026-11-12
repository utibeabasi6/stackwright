package builders

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stackwright/stackwright/internal/seal"
)

// CertLifetime is how long a build server's certificate lasts. The builder
// renews it on its stream after half of that.
const CertLifetime = 24 * time.Hour

var errBadCSR = errors.New("the certificate request is malformed or its signature is wrong")

// CA signs build server certificates and the builder port's own
// certificate. Builders pin it by its SHA-256 (adr/0021).
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu     sync.Mutex
	server *tls.Certificate
}

// LoadCA reads the CA, or makes it on first use.
func LoadCA(ctx context.Context, pool *pgxpool.Pool, kek seal.KEK) (*CA, error) {
	var certDER, sealed []byte
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT cert, key FROM builder_ca`).Scan(&certDER, &sealed)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		now := time.Now()
		certDER, err = x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber:          serial(),
			Subject:               pkix.Name{CommonName: "Stackwright builder CA"},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.AddDate(20, 0, 0),
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
			MaxPathLenZero:        true,
		}, &x509.Certificate{Subject: pkix.Name{CommonName: "Stackwright builder CA"}}, key.Public(), key)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if sealed, err = kek.Seal(ctx, tx, "", keyDER); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO builder_ca (cert, key) VALUES ($1, $2)`, certDER, sealed)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("load the builder CA: %w", err)
	}
	var keyDER []byte
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		keyDER, err = kek.Open(ctx, tx, "", sealed)
		return err
	}); err != nil {
		return nil, fmt.Errorf("open the builder CA key: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("the builder CA key is not ECDSA")
	}
	return &CA{cert: cert, key: ecKey}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

func (c *CA) Certificate() []byte { return c.cert.Raw }

// Fingerprint is the hex SHA-256 of the CA certificate.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// issue signs a client certificate for csr, names the build server in it,
// and records its serial. tx needs no org setting.
func (c *CA) issue(ctx context.Context, tx pgx.Tx, csrDER []byte, serverID, orgID string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil, errBadCSR
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: serverID},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(CertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO build_server_certs (serial, build_server_id, org_id, expires_at)
		VALUES ($1, $2, nullif($3, '')::uuid, $4)`, tmpl.SerialNumber.Text(16), serverID, orgID, tmpl.NotAfter); err != nil {
		return nil, err
	}
	return der, nil
}

// TLSConfig serves the builder port. A client certificate is optional at
// the handshake, because Enrol has none; the interceptor requires one for
// Connect.
func (c *CA) TLSConfig(hosts []string) *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(c.cert)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  roots,
		NextProtos: []string{"h2"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.serverCertificate(hosts)
		},
	}
}

// serverCertificate lasts 30 days and is made again a week before it ends.
func (c *CA) serverCertificate(hosts []string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server != nil && time.Until(c.server.Leaf.NotAfter) > 7*24*time.Hour {
		return c.server, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c.server = &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}
	return c.server, nil
}
