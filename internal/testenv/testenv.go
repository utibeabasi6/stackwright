package testenv

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

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
	if err := db.SetAppPassword(ctx, owner, os.Getenv("STACKWRIGHT_APP_DB_PASSWORD")); err != nil {
		t.Fatal(err)
	}
	app, err := db.Open(ctx, config(name, "stackwright_app", os.Getenv("STACKWRIGHT_APP_DB_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return DB{Owner: owner, App: app}
}
