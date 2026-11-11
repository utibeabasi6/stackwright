package engine

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

// From https://get.opentofu.org/opentofu.asc.
//
//go:embed opentofu.asc
var openTofuKey []byte

// From https://www.hashicorp.com/.well-known/pgp-key.txt.
//
//go:embed hashicorp.asc
var hashiCorpKey []byte

var (
	ErrSignature = errors.New("the checksum file is not signed by the engine's release key")
	ErrChecksum  = errors.New("the engine archive does not match its signed checksum")

	validVersion  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	validPlatform = regexp.MustCompile(`^linux_(amd64|arm64)$`)
)

type release struct {
	key         []byte
	fingerprint string
	baseURL     string
	prefix      string
	sigSuffix   string
}

func (r release) url(version, file string) string {
	return strings.ReplaceAll(r.baseURL, "{version}", version) + "/" + fmt.Sprintf("%s_%s_%s", r.prefix, version, file)
}

var upstream = map[string]release{
	"opentofu": {
		key: openTofuKey, fingerprint: "E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80",
		baseURL: "https://github.com/opentofu/opentofu/releases/download/v{version}",
		prefix:  "tofu", sigSuffix: ".gpgsig",
	},
	"terraform": {
		key: hashiCorpKey, fingerprint: "C874011F0AB405110D02105534365D9472D7468F",
		baseURL: "https://releases.hashicorp.com/terraform/{version}",
		prefix:  "terraform", sigSuffix: ".sig",
	},
}

type Binary struct {
	Name      string
	ObjectKey string
	SHA256    string
}

// Mirror keeps checked engine binaries in S3. Pool needs no org setting.
type Mirror struct {
	Pool   *pgxpool.Pool
	S3     *minio.Client
	Bucket string
	HTTP   *http.Client

	releases map[string]release
}

// Ensure returns the binary for an engine version on a platform such as
// linux_arm64. The first call for one fetches it from the upstream release.
func (m *Mirror) Ensure(ctx context.Context, engine, version, platform string) (Binary, error) {
	r, ok := m.release(engine)
	if !ok {
		return Binary{}, fmt.Errorf("unknown engine %q", engine)
	}
	if !validVersion.MatchString(version) || !validPlatform.MatchString(platform) {
		return Binary{}, fmt.Errorf("bad engine version %q or platform %q", version, platform)
	}
	b := Binary{Name: r.prefix}
	err := m.Pool.QueryRow(ctx, `SELECT object_key, sha256 FROM engines WHERE engine = $1 AND version = $2 AND platform = $3`,
		engine, version, platform).Scan(&b.ObjectKey, &b.SHA256)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Binary{}, err
	}

	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(r.key))
	if err != nil {
		return Binary{}, err
	}
	if len(keyring) != 1 || fmt.Sprintf("%X", keyring[0].PrimaryKey.Fingerprint) != r.fingerprint {
		return Binary{}, fmt.Errorf("the %s release key does not match its pinned fingerprint", engine)
	}
	sums, err := m.get(ctx, r.url(version, "SHA256SUMS"))
	if err != nil {
		return Binary{}, err
	}
	sig, err := m.get(ctx, r.url(version, "SHA256SUMS"+r.sigSuffix))
	if err != nil {
		return Binary{}, err
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		return Binary{}, fmt.Errorf("%w: %w", ErrSignature, err)
	}
	zipName := fmt.Sprintf("%s_%s_%s.zip", r.prefix, version, platform)
	want := ""
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		if sum, file, ok := strings.Cut(scanner.Text(), "  "); ok && file == zipName {
			want = sum
		}
	}
	if want == "" {
		return Binary{}, fmt.Errorf("the checksum file has no entry for %s", zipName)
	}
	archive, err := m.get(ctx, r.url(version, platform+".zip"))
	if err != nil {
		return Binary{}, err
	}
	if hexSHA256(archive) != want {
		return Binary{}, ErrChecksum
	}
	bin, err := unzip(archive, r.prefix)
	if err != nil {
		return Binary{}, err
	}

	b.SHA256 = hexSHA256(bin)
	b.ObjectKey = fmt.Sprintf("engines/%s/%s/%s/%s", engine, version, platform, r.prefix)
	if _, err := m.S3.PutObject(ctx, m.Bucket, b.ObjectKey, bytes.NewReader(bin), int64(len(bin)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return Binary{}, fmt.Errorf("store engine binary: %w", err)
	}
	if _, err := m.Pool.Exec(ctx, `INSERT INTO engines (engine, version, platform, sha256, size, object_key)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		engine, version, platform, b.SHA256, len(bin), b.ObjectKey); err != nil {
		return Binary{}, err
	}
	return b, nil
}

func (m *Mirror) release(engine string) (release, bool) {
	if m.releases != nil {
		r, ok := m.releases[engine]
		return r, ok
	}
	r, ok := upstream[engine]
	return r, ok
}

func (m *Mirror) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func unzip(archive []byte, name string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(io.LimitReader(rc, 512<<20))
	}
	return nil, fmt.Errorf("the engine archive has no %s binary", name)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
