package testenv

import (
	"context"
	"os"
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

func PostgresConfig(t *testing.T) db.Config {
	Require(t)
	return db.Config{
		Host:     "127.0.0.1",
		Port:     15432,
		User:     "stackwright",
		Database: "stackwright",
		Password: os.Getenv("STACKWRIGHT_POSTGRES_PASSWORD"),
		SSLMode:  "disable",
	}
}

func Postgres(t *testing.T) *pgxpool.Pool {
	pool, err := db.Open(context.Background(), PostgresConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
