package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/minio/minio-go/v7"

	"github.com/utibeabasi6/stackwright/internal/testenv"
)

type fakeRelease struct {
	files map[string][]byte
	gets  atomic.Int32
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.gets.Add(1)
	b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

func newKey(t *testing.T) (*openpgp.Entity, []byte) {
	t.Helper()
	e, err := openpgp.NewEntity("release", "", "release@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return e, buf.Bytes()
}

func zipped(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create(name)
	_, _ = f.Write(body)
	_ = zw.Close()
	return buf.Bytes()
}

func fixture(t *testing.T, signer *openpgp.Entity, archive []byte) *fakeRelease {
	t.Helper()
	sums := fmt.Sprintf("%s  tofu_1.10.7_linux_arm64.zip\n%s  tofu_1.10.7_linux_amd64.zip\n", hexSHA256(archive), strings.Repeat("0", 64))
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, signer, strings.NewReader(sums), nil); err != nil {
		t.Fatal(err)
	}
	return &fakeRelease{files: map[string][]byte{
		"tofu_1.10.7_SHA256SUMS":        []byte(sums),
		"tofu_1.10.7_SHA256SUMS.gpgsig": sig.Bytes(),
		"tofu_1.10.7_linux_arm64.zip":   archive,
	}}
}

func mirror(t *testing.T, releases http.Handler, key []byte, fingerprint string) (*Mirror, *minio.Client, string) {
	t.Helper()
	db := testenv.New(t)
	s3, bucket := testenv.S3(t)
	srv := httptest.NewServer(releases)
	t.Cleanup(srv.Close)
	return &Mirror{Pool: db.App, S3: s3, Bucket: bucket, HTTP: srv.Client(), releases: map[string]release{
		"opentofu": {key: key, fingerprint: fingerprint, baseURL: srv.URL, prefix: "tofu", sigSuffix: ".gpgsig",
			index: srv.URL + "/api.json", versions: upstream["opentofu"].versions},
	}}, s3, bucket
}

func TestEnsureStoresACheckedBinaryOnce(t *testing.T) {
	signer, pub := newKey(t)
	binary := []byte("#!engine\n")
	up := fixture(t, signer, zipped(t, "tofu", binary))
	m, s3, bucket := mirror(t, up, pub, fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint))
	ctx := context.Background()

	b, err := m.Ensure(ctx, "opentofu", "1.10.7", "linux_arm64")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "tofu" || b.SHA256 != hexSHA256(binary) || b.ObjectKey != "engines/opentofu/1.10.7/linux_arm64/tofu" {
		t.Errorf("binary: %+v", b)
	}
	obj, err := s3.GetObject(ctx, bucket, b.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(obj); !bytes.Equal(got, binary) {
		t.Errorf("stored %q", got)
	}

	gets := up.gets.Load()
	again, err := m.Ensure(ctx, "opentofu", "1.10.7", "linux_arm64")
	if err != nil || again != b {
		t.Errorf("second call: %+v %v", again, err)
	}
	if up.gets.Load() != gets {
		t.Error("the second call went upstream")
	}
}

func TestEnsureRefusesUnsignedOrChangedReleases(t *testing.T) {
	signer, pub := newKey(t)
	other, _ := newKey(t)
	fingerprint := fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint)
	archive := zipped(t, "tofu", []byte("#!engine\n"))

	cases := map[string]struct {
		upstream    *fakeRelease
		fingerprint string
		want        error
	}{
		"signed by another key": {upstream: fixture(t, other, archive), fingerprint: fingerprint, want: ErrSignature},
		"changed archive": {upstream: func() *fakeRelease {
			f := fixture(t, signer, archive)
			f.files["tofu_1.10.7_linux_arm64.zip"] = zipped(t, "tofu", []byte("#!evil\n"))
			return f
		}(), fingerprint: fingerprint, want: ErrChecksum},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, _, _ := mirror(t, c.upstream, pub, c.fingerprint)
			_, err := m.Ensure(context.Background(), "opentofu", "1.10.7", "linux_arm64")
			if !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
			var n int
			_ = m.Pool.QueryRow(context.Background(), `SELECT count(*) FROM engines`).Scan(&n)
			if n != 0 {
				t.Errorf("%d engines recorded", n)
			}
		})
	}

	t.Run("key file does not match the pinned fingerprint", func(t *testing.T) {
		m, _, _ := mirror(t, fixture(t, signer, archive), pub, strings.Repeat("A", 40))
		if _, err := m.Ensure(context.Background(), "opentofu", "1.10.7", "linux_arm64"); err == nil || !strings.Contains(err.Error(), "pinned fingerprint") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("bad version", func(t *testing.T) {
		m, _, _ := mirror(t, fixture(t, signer, archive), pub, fingerprint)
		if _, err := m.Ensure(context.Background(), "opentofu", "1.10.7/../../x", "linux_arm64"); err == nil {
			t.Error("accepted a version with a path in it")
		}
	})
}

func TestEnsureFromUpstream(t *testing.T) {
	if os.Getenv("STACKWRIGHT_TEST_ENGINES") == "" {
		t.Skip("set STACKWRIGHT_TEST_ENGINES=1 to download real releases")
	}
	db := testenv.New(t)
	s3, bucket := testenv.S3(t)
	m := &Mirror{Pool: db.App, S3: s3, Bucket: bucket, HTTP: http.DefaultClient}
	for engine, version := range map[string]string{"opentofu": "1.10.7", "terraform": "1.5.7"} {
		b, err := m.Ensure(context.Background(), engine, version, "linux_amd64")
		if err != nil {
			t.Errorf("%s %s: %v", engine, version, err)
			continue
		}
		t.Logf("%s %s: %s", engine, version, b.SHA256)
	}
}

func TestVersionsAreStableAndNewestFirst(t *testing.T) {
	up := &fakeRelease{files: map[string][]byte{
		"api.json": []byte(`{"versions":[{"id":"1.9.0"},{"id":"1.10.7"},{"id":"1.11.0-beta1"},{"id":"1.10.10"},{"id":"1.9.0"}]}`),
	}}
	m, _, _ := mirror(t, up, nil, "")
	got, err := m.Versions(context.Background(), "opentofu")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[1.10.10 1.10.7 1.9.0]" {
		t.Errorf("versions: %v", got)
	}
	if _, err := m.Versions(context.Background(), "opentofu"); err != nil || up.gets.Load() != 1 {
		t.Errorf("a second call fetched the index again: %d gets, %v", up.gets.Load(), err)
	}
}
