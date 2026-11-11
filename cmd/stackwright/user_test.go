package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/testenv"
)

func TestResetPassword(t *testing.T) {
	ctx := context.Background()
	d := testenv.New(t)
	if _, err := d.Owner.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES (gen_random_uuid(), 'kim@a.test', $1)`,
		auth.HashPassword("old password here")); err != nil {
		t.Fatal(err)
	}
	if _, err := resetPassword(ctx, d.Owner, "nobody@a.test"); !errors.Is(err, errNoUser) {
		t.Fatalf("unknown email: %v", err)
	}
	password, err := resetPassword(ctx, d.Owner, "Kim@A.test")
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := d.Owner.QueryRow(ctx, `SELECT password_hash FROM users WHERE email = 'kim@a.test'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !auth.PasswordMatches(hash, password) || auth.PasswordMatches(hash, "old password here") {
		t.Fatal("the stored hash does not match the new password only")
	}
}
