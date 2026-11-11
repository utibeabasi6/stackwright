package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/testenv"
)

func TestOpenRefusesAnEmptyPassword(t *testing.T) {
	_, err := db.Open(context.Background(), db.Config{Host: "127.0.0.1", Port: 1, User: "x", Database: "x"})
	if !errors.Is(err, db.ErrNoPassword) {
		t.Fatalf("err = %v, want ErrNoPassword", err)
	}
}

func TestOpenRefusesAWrongPassword(t *testing.T) {
	cfg := testenv.PostgresConfig(t)
	cfg.Password = "wrong"
	if _, err := db.Open(context.Background(), cfg); err == nil {
		t.Fatal("connected with a wrong password")
	}
}

func TestMigrateRunsTwice(t *testing.T) {
	pool := testenv.Postgres(t)
	for range 2 {
		if err := db.Migrate(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
	}
}
