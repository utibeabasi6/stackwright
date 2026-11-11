package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

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
	cfg := testenv.OwnerConfig(t)
	cfg.Password = "wrong"
	if _, err := db.Open(context.Background(), cfg); err == nil {
		t.Fatal("connected with a wrong password")
	}
}

func TestMigrateRunsTwice(t *testing.T) {
	d := testenv.New(t)
	if err := db.Migrate(context.Background(), d.Owner); err != nil {
		t.Fatal(err)
	}
}

func TestSetAppPasswordRefusesQuotes(t *testing.T) {
	d := testenv.New(t)
	err := db.SetAppPassword(context.Background(), d.Owner, "abcdefghijklmnopqrst'; DROP TABLE users; --")
	if !errors.Is(err, db.ErrBadPassword) {
		t.Fatalf("err = %v, want ErrBadPassword", err)
	}
}

func TestTheAppRoleIsBoundByRowLevelSecurity(t *testing.T) {
	ctx := context.Background()
	d := testenv.New(t)
	a, b := uuid.NewString(), uuid.NewString()
	if _, err := d.Owner.Exec(ctx, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'a', 'A'), ($2, 'b', 'B')`, a, b); err != nil {
		t.Fatal(err)
	}

	var super, bypass bool
	if err := d.App.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("stackwright_app: superuser=%v bypassrls=%v", super, bypass)
	}

	var n int
	if err := d.App.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("with no settings the app sees %d orgs (err %v), want 0", n, err)
	}

	tx, err := d.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('stackwright.org_id', $1, true)`, a); err != nil {
		t.Fatal(err)
	}
	var slug string
	if err := tx.QueryRow(ctx, `SELECT string_agg(slug, ',') FROM orgs`).Scan(&slug); err != nil || slug != "a" {
		t.Fatalf("in org a the app sees %q (err %v), want a", slug, err)
	}

	for _, sql := range []string{
		`ALTER TABLE orgs DISABLE ROW LEVEL SECURITY`,
		`ALTER TABLE orgs NO FORCE ROW LEVEL SECURITY`,
		`DROP POLICY orgs_select ON orgs`,
		`SET ROLE stackwright`,
	} {
		if _, err := d.App.Exec(ctx, sql); err == nil {
			t.Errorf("the app ran %q", sql)
		}
	}
	conn, err := d.App.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET row_security = off`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM orgs`).Scan(&n); err == nil {
		t.Errorf("with row_security off the app read %d orgs; want an error", n)
	}
	if _, err := conn.Exec(ctx, `RESET row_security`); err != nil {
		t.Fatal(err)
	}
}
