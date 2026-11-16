package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/utibeabasi6/stackwright/internal/db"
	"github.com/utibeabasi6/stackwright/internal/server"
	"github.com/utibeabasi6/stackwright/internal/state"
	"github.com/utibeabasi6/stackwright/internal/testenv"
)

// The backup is the one in docs/operations.md.
func TestRestoreFromABackup(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, nil)
	owner := w.setup()
	w.project(owner, "a", "net", "prod")
	token := w.token(owner, "a")
	doc := stateDoc(1, "L1", "a", "b")
	if code, body := w.backend(http.MethodPost, "/state/a/net/prod", token, doc); code != http.StatusOK {
		t.Fatalf("write: %d %s", code, body)
	}

	out, err := exec.Command("docker", "ps", "-q", "--filter", "publish=15432").Output()
	container := strings.TrimSpace(string(out))
	if err != nil || container == "" {
		t.Fatalf("find the Postgres container: %v", err)
	}
	from := w.owner.Config().ConnConfig.Database
	var dump bytes.Buffer
	cmd := exec.Command("docker", "exec", container, "pg_dump", "-U", "stackwright", "-Fc", from)
	cmd.Stdout, cmd.Stderr = &dump, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	to := "test_" + strings.ToLower(rand.Text()[:12])
	if _, err := w.owner.Exec(ctx, "CREATE DATABASE "+to); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.owner.Exec(context.Background(), "DROP DATABASE "+to+" WITH (FORCE)") })
	cfg := testenv.OwnerConfig(t)
	cfg.Database = to
	fresh, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Migrate(ctx, fresh)
	fresh.Close()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("docker", "exec", "-i", container, "pg_restore", "-U", "stackwright", "--clean", "--if-exists",
		"--exit-on-error", "-d", to)
	cmd.Stdin, cmd.Stderr = &dump, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_restore: %v", err)
	}
	cfg.User, cfg.Password = "stackwright_app", os.Getenv("STACKWRIGHT_APP_DB_PASSWORD")
	app, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	s3, bucket := testenv.S3(t)
	for o := range w.s3.ListObjects(ctx, w.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if _, err := s3.CopyObject(ctx, minio.CopyDestOptions{Bucket: bucket, Object: o.Key},
			minio.CopySrcOptions{Bucket: w.bucket, Object: o.Key}); err != nil {
			t.Fatal(err)
		}
	}
	if tag, err := w.owner.Exec(ctx, "DELETE FROM state_versions"); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("clear the first database: %v %v", tag, err)
	}

	restored := w.cfg
	restored.Pool, restored.Bucket = app, bucket
	restored.State = state.Store{S3: s3, Bucket: bucket, KEK: w.cfg.KEK}
	h := server.New(restored)
	w.handler.Store(&h)
	if code, body := w.backend(http.MethodGet, "/state/a/net/prod", token, ""); code != http.StatusOK || body != doc {
		t.Errorf("the restored server: %d %q", code, body)
	}
}
