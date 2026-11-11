package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	return env
}

func TestInitWritesPrivateRandomSecrets(t *testing.T) {
	out := filepath.Join(t.TempDir(), ".env")
	if err := runInit([]string{"-o", out}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	env := readEnv(t, out)
	if len(env) != len(initSecrets) {
		t.Fatalf("wrote %d secrets, want %d", len(env), len(initSecrets))
	}
	seen := map[string]bool{}
	for k, v := range env {
		if len(v) < 20 || seen[v] {
			t.Errorf("%s = %q is short or repeated", k, v)
		}
		seen[v] = true
		if k != "STACKWRIGHT_KEK" && (v[0] < 'a' || v[0] > 'z') {
			t.Errorf("%s starts with %q, want a letter", k, v[0])
		}
	}
}

func TestInitAddsOnlyWhatIsMissing(t *testing.T) {
	out := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(out, []byte("STACKWRIGHT_POSTGRES_PASSWORD=keepme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-o", out}); err != nil {
		t.Fatal(err)
	}
	env := readEnv(t, out)
	if env["STACKWRIGHT_POSTGRES_PASSWORD"] != "keepme" {
		t.Errorf("init changed an existing secret")
	}
	if len(env) != len(initSecrets) {
		t.Errorf("have %d secrets, want %d", len(env), len(initSecrets))
	}
	before, _ := os.ReadFile(out)
	if err := runInit([]string{"-o", out}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(out); string(after) != string(before) {
		t.Errorf("a second init changed the file")
	}
}
