// Package config reads the settings that the binaries take from the
// environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Secret reads k, or the file that k_FILE names, as compose secrets give it.
func Secret(k string) (string, error) {
	if file := os.Getenv(k + "_FILE"); file != "" {
		b, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", k, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(k), nil
}
