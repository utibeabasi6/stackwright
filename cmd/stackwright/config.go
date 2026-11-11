package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stackwright/stackwright/internal/db"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func secretEnv(k string) (string, error) {
	if file := os.Getenv(k + "_FILE"); file != "" {
		b, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", k, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(k), nil
}

func dbConfig(user, passwordVar string) (db.Config, error) {
	port, err := strconv.Atoi(env("STACKWRIGHT_POSTGRES_PORT", "5432"))
	if err != nil {
		return db.Config{}, fmt.Errorf("STACKWRIGHT_POSTGRES_PORT: %w", err)
	}
	password, err := secretEnv(passwordVar)
	if err != nil {
		return db.Config{}, err
	}
	return db.Config{
		Host:     env("STACKWRIGHT_POSTGRES_HOST", "postgres"),
		Port:     port,
		User:     user,
		Database: env("STACKWRIGHT_POSTGRES_DB", "stackwright"),
		Password: password,
		SSLMode:  env("STACKWRIGHT_POSTGRES_SSLMODE", "prefer"),
	}, nil
}
