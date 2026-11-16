package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stackwright/stackwright/internal/config"
	"github.com/stackwright/stackwright/internal/db"
	"github.com/stackwright/stackwright/internal/seal"
)

// loadKEK reads STACKWRIGHT_KEK and, while a rotation runs,
// STACKWRIGHT_KEK_PREVIOUS.
func loadKEK() (seal.KEK, error) {
	text, err := config.Secret("STACKWRIGHT_KEK")
	if err != nil {
		return seal.KEK{}, err
	}
	kek, err := seal.ParseKEK(text)
	if err != nil {
		return seal.KEK{}, fmt.Errorf("STACKWRIGHT_KEK: %w", err)
	}
	previous, err := config.Secret("STACKWRIGHT_KEK_PREVIOUS")
	if err != nil || previous == "" {
		return kek, err
	}
	kek, err = kek.WithPrevious(previous)
	if err != nil {
		return seal.KEK{}, fmt.Errorf("STACKWRIGHT_KEK_PREVIOUS: %w", err)
	}
	return kek, nil
}

func runRotateKEK(args []string) error {
	fl := flag.NewFlagSet("rotate-kek", flag.ExitOnError)
	_ = fl.Parse(args)

	ctx := context.Background()
	kek, err := loadKEK()
	if err != nil {
		return err
	}
	owner, err := dbConfig("stackwright", "STACKWRIGHT_POSTGRES_PASSWORD")
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, owner)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.CheckOwner(ctx, pool); err != nil {
		return err
	}
	var n int
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		n, err = kek.Rotate(ctx, tx)
		return err
	})
	if errors.Is(err, seal.ErrWrongKEK) {
		return fmt.Errorf("%w; set STACKWRIGHT_KEK_PREVIOUS to the KEK that wrapped it", err)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%d data keys now use STACKWRIGHT_KEK; STACKWRIGHT_KEK_PREVIOUS can go\n", n)
	return nil
}
