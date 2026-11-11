package testenv

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/stackwright/stackwright/internal/db"
)

func Require(t *testing.T) {
	t.Helper()
	if os.Getenv("STACKWRIGHT_TEST_INFRA") == "" {
		t.Skip("set STACKWRIGHT_TEST_INFRA=1 and run make up")
	}
}

func config(database, user, password string) db.Config {
	return db.Config{Host: "127.0.0.1", Port: 15432, User: user, Database: database, Password: password, SSLMode: "disable"}
}

func OwnerConfig(t *testing.T) db.Config {
	Require(t)
	return config("stackwright", "stackwright", os.Getenv("STACKWRIGHT_POSTGRES_PASSWORD"))
}

type DB struct {
	Owner *pgxpool.Pool
	App   *pgxpool.Pool
}

func New(t *testing.T) DB {
	t.Helper()
	ctx := context.Background()
	admin, err := db.Open(ctx, OwnerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "test_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })

	owner, err := db.Open(ctx, config(name, "stackwright", os.Getenv("STACKWRIGHT_POSTGRES_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := db.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	// The login is shared by every test database, and parallel tests that
	// alter it at once fail with "tuple concurrently updated".
	for try := 0; ; try++ {
		err := db.SetAppPassword(ctx, owner, os.Getenv("STACKWRIGHT_APP_DB_PASSWORD"))
		if err == nil {
			break
		}
		if try == 4 {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	app, err := db.Open(ctx, config(name, "stackwright_app", os.Getenv("STACKWRIGHT_APP_DB_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return DB{Owner: owner, App: app}
}

func S3(t *testing.T) (*minio.Client, string) {
	t.Helper()
	Require(t)
	ctx := context.Background()
	c, err := minio.New("127.0.0.1:19000", &minio.Options{
		Creds: credentials.NewStaticV4(os.Getenv("STACKWRIGHT_S3_ACCESS_KEY"), os.Getenv("STACKWRIGHT_S3_SECRET_KEY"), ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "test-" + strings.ToLower(rand.Text()[:12])
	if err := c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for o := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = c.RemoveObject(ctx, bucket, o.Key, minio.RemoveObjectOptions{})
		}
		_ = c.RemoveBucket(ctx, bucket)
	})
	return c, bucket
}
