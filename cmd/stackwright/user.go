package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/utibeabasi6/stackwright/internal/auth"
	"github.com/utibeabasi6/stackwright/internal/db"
)

var errNoUser = errors.New("no user has that email")

func runUser(args []string) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New("usage: stackwright user reset-password -email <address>")
	}
	fl := flag.NewFlagSet("user reset-password", flag.ExitOnError)
	email := fl.String("email", "", "the user's email")
	_ = fl.Parse(args[1:])

	ctx := context.Background()
	cfg, err := dbConfig("stackwright", "STACKWRIGHT_POSTGRES_PASSWORD")
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	password, err := resetPassword(ctx, pool, *email)
	if err != nil {
		return err
	}
	fmt.Printf("new password for %s: %s\nIt is shown once. Every session, CLI token and API token of this user was ended.\n", *email, password)
	return nil
}

func resetPassword(ctx context.Context, pool *pgxpool.Pool, email string) (string, error) {
	password := rand.Text()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `UPDATE users SET password_hash = $2 WHERE lower(email) = lower($1) RETURNING id`,
			email, auth.HashPassword(password)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoUser
		}
		if err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM sessions WHERE user_id = $1`,
			`UPDATE cli_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
			`UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor_kind, actor_id, action, target)
			VALUES ('system', 'cli', 'user.reset_password', $1)`, "user:"+id)
		return err
	})
	return password, err
}
