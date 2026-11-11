package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	b, _ := os.ReadFile(out)
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || len(v) < 20 || v[0] < 'a' || v[0] > 'z' {
			t.Errorf("bad line %q", line)
		}
		if seen[v] {
			t.Errorf("%s repeats a value", k)
		}
		seen[v] = true
	}
	if len(seen) != len(initSecrets) {
		t.Errorf("wrote %d secrets, want %d", len(seen), len(initSecrets))
	}
}

func TestInitNeverOverwrites(t *testing.T) {
	out := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(out, []byte("KEEP=me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-o", out}); err == nil {
		t.Fatal("init overwrote an existing file")
	}
	if b, _ := os.ReadFile(out); string(b) != "KEEP=me\n" {
		t.Fatalf("file changed to %q", b)
	}
}
