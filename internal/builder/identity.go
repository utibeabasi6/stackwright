package builder

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"connectrpc.com/connect"

	v1 "github.com/utibeabasi6/stackwright/gen/stackwright/v1"
	"github.com/utibeabasi6/stackwright/gen/stackwright/v1/stackwrightv1connect"
)

var errCAPin = errors.New("the server's CA does not match the pinned SHA-256")

// Identity is a build server's key, its certificate and the CA it trusts.
type Identity struct {
	ServerID   string
	Name       string
	BuilderURL string
	ca         *x509.Certificate
	key        *ecdsa.PrivateKey
	// path is where the identity is kept, or empty for one in memory.
	path string

	mu   sync.Mutex
	cert *x509.Certificate
}

type stored struct {
	ServerID   string `json:"build_server_id"`
	Name       string `json:"name"`
	BuilderURL string `json:"builder_url"`
	CA         []byte `json:"ca"`
	Key        []byte `json:"key"`
	Cert       []byte `json:"cert"`
}

// Enrol spends token and returns the new identity. path is where to keep
// it, or empty to keep it in memory. builderURL replaces the server's
// answer when it is set, for a builder that reaches the port by another
// name.
func Enrol(ctx context.Context, client stackwrightv1connect.BuilderServiceClient, token string, slots int32, labels map[string]string,
	path, builderURL string) (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	resp, err := client.Enrol(ctx, connect.NewRequest(&v1.EnrolRequest{Token: token, Csr: csr, Slots: slots, Labels: labels}))
	if err != nil {
		return nil, fmt.Errorf("enrol: %w", err)
	}
	ca, err := x509.ParseCertificate(resp.Msg.GetCaCertificate())
	if err != nil {
		return nil, err
	}
	id := &Identity{ServerID: resp.Msg.GetBuildServerId(), Name: resp.Msg.GetName(), BuilderURL: resp.Msg.GetBuilderUrl(), ca: ca, key: key, path: path}
	if builderURL != "" {
		id.BuilderURL = builderURL
	}
	if err := id.setCertificate(resp.Msg.GetCertificate()); err != nil {
		return nil, err
	}
	return id, nil
}

// Load reads an identity that Enrol kept at path.
func Load(path string) (*Identity, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the builder's own state file
	if err != nil {
		return nil, err
	}
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ca, err := x509.ParseCertificate(s.CA)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(s.Key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(s.Cert)
	if err != nil {
		return nil, err
	}
	return &Identity{ServerID: s.ServerID, Name: s.Name, BuilderURL: s.BuilderURL, ca: ca, key: key, cert: cert, path: path}, nil
}

func (id *Identity) setCertificate(der []byte) error {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	if err := cert.CheckSignatureFrom(id.ca); err != nil {
		return fmt.Errorf("the certificate is not from the pinned CA: %w", err)
	}
	id.mu.Lock()
	id.cert = cert
	id.mu.Unlock()
	if id.path == "" {
		return nil
	}
	key, err := x509.MarshalECPrivateKey(id.key)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(stored{ServerID: id.ServerID, Name: id.Name, BuilderURL: id.BuilderURL, CA: id.ca.Raw, Key: key, Cert: der})
	tmp := id.path + ".new"
	if err := os.MkdirAll(filepath.Dir(id.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, id.path)
}

func (id *Identity) certificate() *x509.Certificate {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.cert
}

func (id *Identity) csr() ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, id.key)
}

// Client dials the builder port with this identity's certificate. Each
// call makes a new transport, so a renewed certificate is used at once.
func (id *Identity) Client() stackwrightv1connect.BuilderServiceClient {
	roots := x509.NewCertPool()
	roots.AddCert(id.ca)
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	transport := &http.Transport{Protocols: &protocols, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert := id.certificate()
			return &tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: id.key, Leaf: cert}, nil
		},
	}}
	return stackwrightv1connect.NewBuilderServiceClient(&http.Client{Transport: transport}, id.BuilderURL)
}

// PinnedClient reaches the builder port before the builder has a
// certificate. It trusts only a chain that ends at the CA whose SHA-256
// is caSHA256.
func PinnedClient(builderURL, caSHA256 string) (stackwrightv1connect.BuilderServiceClient, error) {
	u, err := url.Parse(builderURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("the builder URL %q is not https://host:port", builderURL)
	}
	verify := func(cs tls.ConnectionState) error {
		var ca *x509.Certificate
		for _, c := range cs.PeerCertificates {
			sum := sha256.Sum256(c.Raw)
			if hex.EncodeToString(sum[:]) == caSHA256 {
				ca = c
			}
		}
		if ca == nil || len(cs.PeerCertificates) == 0 {
			return errCAPin
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, DNSName: u.Hostname()})
		return err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // VerifyConnection checks the chain against the pinned CA
		VerifyConnection:   verify,
	}}
	return stackwrightv1connect.NewBuilderServiceClient(&http.Client{Transport: transport}, builderURL), nil
}

// PlainClient reaches the server without TLS, as the built-in builder does
// inside compose to enrol.
func PlainClient(serverURL string) stackwrightv1connect.BuilderServiceClient {
	return stackwrightv1connect.NewBuilderServiceClient(http.DefaultClient, serverURL)
}
