package main

import (
	"fmt"
	"strconv"

	"github.com/utibeabasi6/stackwright/internal/config"
	"github.com/utibeabasi6/stackwright/internal/db"
)

func dbConfig(user, passwordVar string) (db.Config, error) {
	port, err := strconv.Atoi(config.Env("STACKWRIGHT_POSTGRES_PORT", "5432"))
	if err != nil {
		return db.Config{}, fmt.Errorf("STACKWRIGHT_POSTGRES_PORT: %w", err)
	}
	password, err := config.Secret(passwordVar)
	if err != nil {
		return db.Config{}, err
	}
	return db.Config{
		Host:     config.Env("STACKWRIGHT_POSTGRES_HOST", "postgres"),
		Port:     port,
		User:     user,
		Database: config.Env("STACKWRIGHT_POSTGRES_DB", "stackwright"),
		Password: password,
		SSLMode:  config.Env("STACKWRIGHT_POSTGRES_SSLMODE", "prefer"),
	}, nil
}
