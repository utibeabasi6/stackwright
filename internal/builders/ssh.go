package builders

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"github.com/stackwright/stackwright/internal/seal"
)

var (
	errHostKey   = errors.New("the host's SSH key does not match the key you confirmed")
	ErrNoDocker  = errors.New("the host has no Docker")
	errNoSystemd = errors.New("the host has no systemd")
	errNoBinary  = errors.New("this server has no builder binary for the host's platform")
)

// Binary is a builder binary that the server hands out.
type Binary struct {
	Path   string
	SHA256 string
}

// LoadBinaries finds stackwright-builder-<platform> files in dir. A missing
// dir gives none.
func LoadBinaries(dir string) (map[string]Binary, error) {
	out := map[string]Binary{}
	for _, p := range []string{"linux_amd64", "linux_arm64"} {
		path := filepath.Join(dir, "stackwright-builder-"+p)
		b, err := os.ReadFile(path) //nolint:gosec // a fixed name in the configured folder
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out[p] = Binary{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	return out, nil
}

// SSHKey returns the org's signer and its authorized_keys line, and makes
// the key on first use. tx needs the org setting.
func SSHKey(ctx context.Context, tx pgx.Tx, kek seal.KEK, orgID, comment string) (ssh.Signer, string, error) {
	var public string
	var sealed []byte
	err := tx.QueryRow(ctx, `SELECT public_key, private_key FROM org_ssh_keys WHERE org_id = $1`, orgID).Scan(&public, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, "", err
		}
		block, err := ssh.MarshalPrivateKey(priv, comment)
		if err != nil {
			return nil, "", err
		}
		signer, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			return nil, "", err
		}
		public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment
		if sealed, err = kek.Seal(ctx, tx, orgID, pem.EncodeToMemory(block)); err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO org_ssh_keys (org_id, public_key, private_key) VALUES ($1, $2, $3)`, orgID, public, sealed); err != nil {
			return nil, "", err
		}
		return signer, public, nil
	}
	if err != nil {
		return nil, "", err
	}
	raw, err := kek.Open(ctx, tx, orgID, sealed)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	return signer, public, err
}

var hostKeyOrder = []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}

var errScanned = errors.New("host key read")

// ScanHostKey reads the key a host offers, preferring Ed25519, which is
// what `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` prints.
func ScanHostKey(ctx context.Context, host string, port int) (key, fingerprint string, err error) {
	var found ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User:              "stackwright",
		HostKeyAlgorithms: hostKeyOrder,
		Timeout:           10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			found = k
			return errScanned
		},
	}
	conn, err := dial(ctx, host, port)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = conn.Close() }()
	_, _, _, err = ssh.NewClientConn(conn, net.JoinHostPort(host, strconv.Itoa(port)), cfg)
	if found == nil {
		return "", "", fmt.Errorf("read the host key of %s: %w", host, err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(found))), ssh.FingerprintSHA256(found), nil
}

func dial(ctx context.Context, host string, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// Target is a host to enrol over SSH.
type Target struct {
	Host    string
	Port    int
	User    string
	HostKey string
	Signer  ssh.Signer
}

// Install is everything the host needs from the server.
type Install struct {
	Binaries   map[string]Binary
	BuilderURL string
	CA         string
	Token      string
	// Step reports each step as it starts.
	Step func(string)
}

// InstallOverSSH puts the builder on the host, enrols it and starts its
// service. It checks the host key before it sends anything, and checks for
// Docker and systemd before it changes anything.
func InstallOverSSH(ctx context.Context, t Target, in Install) error {
	want, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
	if err != nil {
		return fmt.Errorf("the confirmed host key: %w", err)
	}
	in.Step("Connecting to " + t.Host)
	conn, err := dial(ctx, t.Host, t.Port)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyAlgorithms: []string{want.Type()},
		Timeout:           10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, got ssh.PublicKey) error {
			if !bytes.Equal(got.Marshal(), want.Marshal()) {
				return errHostKey
			}
			return nil
		},
	})
	if errors.Is(err, errHostKey) {
		return errHostKey
	}
	if err != nil {
		return fmt.Errorf("sign in to %s as %s: %w", addr, t.User, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }()

	sudo := ""
	if t.User != "root" {
		sudo = "sudo -n "
	}
	run := func(cmd string, stdin io.Reader) (string, error) {
		s, err := client.NewSession()
		if err != nil {
			return "", err
		}
		defer func() { _ = s.Close() }()
		s.Stdin = stdin
		out, err := s.CombinedOutput(cmd)
		return strings.TrimSpace(string(out)), err
	}

	in.Step("Checking the host")
	arch, err := run("uname -m", nil)
	if err != nil {
		return fmt.Errorf("uname: %w: %s", err, arch)
	}
	platform := map[string]string{"x86_64": "linux_amd64", "aarch64": "linux_arm64", "arm64": "linux_arm64"}[arch]
	bin, ok := in.Binaries[platform]
	if !ok {
		return fmt.Errorf("%w: %s", errNoBinary, arch)
	}
	if out, err := run("command -v docker", nil); err != nil || out == "" {
		return ErrNoDocker
	}
	if out, err := run(sudo+"docker info --format '{{.ServerVersion}}'", nil); err != nil {
		return fmt.Errorf("%s cannot reach Docker on the host: %s", t.User, out)
	}
	if out, err := run("command -v systemctl", nil); err != nil || out == "" {
		return errNoSystemd
	}

	in.Step("Copying the builder")
	f, err := os.Open(bin.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	const tmp = "/usr/local/bin/.stackwright-builder.new"
	if out, err := run(sudo+"sh -c 'cat > "+tmp+" && echo \""+bin.SHA256+"  "+tmp+"\" | sha256sum -c - >/dev/null && chmod 0755 "+tmp+
		" && mv "+tmp+" /usr/local/bin/stackwright-builder'", f); err != nil {
		return fmt.Errorf("copy the builder: %w: %s", err, out)
	}

	in.Step("Enrolling")
	if out, err := run(sudo+"/usr/local/bin/stackwright-builder enrol --server "+shellQuote(in.BuilderURL)+" --ca-sha256 "+in.CA,
		strings.NewReader(in.Token+"\n")); err != nil {
		return fmt.Errorf("enrol: %w: %s", err, out)
	}

	in.Step("Starting the builder service")
	if out, err := run(sudo+"/usr/local/bin/stackwright-builder install", nil); err != nil {
		return fmt.Errorf("install the service: %w: %s", err, out)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
