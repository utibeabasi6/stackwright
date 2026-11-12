package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/stackwright/stackwright/internal/config"
	"github.com/stackwright/stackwright/internal/db"
)

func runMigrate(args []string) error {
	fl := flag.NewFlagSet("migrate", flag.ExitOnError)
	_ = fl.Parse(args)

	ctx := context.Background()
	owner, err := dbConfig("stackwright", "STACKWRIGHT_POSTGRES_PASSWORD")
	if err != nil {
		return err
	}
	appPassword, err := config.Secret("STACKWRIGHT_APP_DB_PASSWORD")
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, owner)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := db.SetAppPassword(ctx, pool, appPassword); err != nil {
		return err
	}
	fmt.Println("migrations applied; stackwright_app can log in")
	return nil
}
