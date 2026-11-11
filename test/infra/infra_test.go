package infra_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/nats-io/nats.go"

	"github.com/stackwright/stackwright/internal/testenv"
)

const (
	natsURL  = "nats://127.0.0.1:14222"
	s3Server = "127.0.0.1:19000"
)

func TestNATSNeedsThePassword(t *testing.T) {
	testenv.Require(t)
	if nc, err := nats.Connect(natsURL, nats.Timeout(3*time.Second)); err == nil {
		nc.Close()
		t.Fatal("connected without a password")
	}
	if nc, err := nats.Connect(natsURL, nats.UserInfo("stackwright", "wrong")); err == nil {
		nc.Close()
		t.Fatal("connected with a wrong password")
	}
	nc, err := nats.Connect(natsURL, nats.UserInfo("stackwright", os.Getenv("STACKWRIGHT_NATS_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AccountInfo(); err != nil {
		t.Fatalf("JetStream is off: %v", err)
	}
}

func s3Client(t *testing.T, secret string) *minio.Client {
	t.Helper()
	c, err := minio.New(s3Server, &minio.Options{
		Creds: credentials.NewStaticV4(os.Getenv("STACKWRIGHT_S3_ACCESS_KEY"), secret, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRustFS(t *testing.T) {
	testenv.Require(t)
	ctx := context.Background()
	c := s3Client(t, os.Getenv("STACKWRIGHT_S3_SECRET_KEY"))
	bucket := "phase0-" + strings.ToLower(rand.Text()[:10])
	if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = c.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = c.RemoveBucket(ctx, bucket)
	})

	t.Run("presigned put and get", func(t *testing.T) {
		body := []byte("plan bytes")
		put, err := c.PresignedPutObject(ctx, bucket, "plan.bin", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, put.String(), bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("presigned PUT = %s", res.Status)
		}

		get, err := c.PresignedGetObject(ctx, bucket, "plan.bin", time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, get.String(), nil)
		res, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
			t.Fatalf("presigned GET = %s %q", res.Status, got)
		}
	})

	t.Run("a presigned URL opens only its own object", func(t *testing.T) {
		get, err := c.PresignedGetObject(ctx, bucket, "plan.bin", time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		other := strings.Replace(get.String(), "/plan.bin?", "/other.bin?", 1)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, other, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("signature reused on another key = %s, want 403", res.Status)
		}
	})

	t.Run("multipart upload", func(t *testing.T) {
		data := make([]byte, 12<<20)
		_, _ = rand.Read(data)
		info, err := c.PutObject(ctx, bucket, "big.log", bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{PartSize: 5 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(info.ETag, "-") {
			t.Errorf("ETag %q has no part count; the upload was not multipart", info.ETag)
		}
		obj, err := c.GetObject(ctx, bucket, "big.log", minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(obj)
		if !bytes.Equal(got, data) {
			t.Fatalf("read back %d bytes, want %d", len(got), len(data))
		}
	})

	t.Run("a wrong secret is refused", func(t *testing.T) {
		if _, err := s3Client(t, "wrong-secret").ListBuckets(ctx); err == nil {
			t.Fatal("listed buckets with a wrong secret")
		}
	})
}
