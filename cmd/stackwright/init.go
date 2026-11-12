package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

var initSecrets = []struct {
	name string
	make func() string
}{
	{"STACKWRIGHT_POSTGRES_PASSWORD", password},
	{"STACKWRIGHT_APP_DB_PASSWORD", password},
	{"STACKWRIGHT_NATS_PASSWORD", password},
	{"STACKWRIGHT_S3_ACCESS_KEY", password},
	{"STACKWRIGHT_S3_SECRET_KEY", password},
	{"STACKWRIGHT_KEK", func() string { return base64.StdEncoding.EncodeToString(randomBytes(32)) }},
	{"STACKWRIGHT_BUILDER_TOKEN", password},
}

// NATS misreads an environment value that starts with a digit.
func password() string { return "s" + rand.Text() }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func runInit(args []string) error {
	fl := flag.NewFlagSet("init", flag.ExitOnError)
	out := fl.String("o", ".env", "file to write")
	_ = fl.Parse(args)

	have := map[string]bool{}
	f, err := os.Open(*out)
	switch {
	case err == nil:
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, _, ok := strings.Cut(sc.Text(), "="); ok {
				have[strings.TrimSpace(k)] = true
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	var b strings.Builder
	var added []string
	for _, s := range initSecrets {
		if !have[s.name] {
			fmt.Fprintf(&b, "%s=%s\n", s.name, s.make())
			added = append(added, s.name)
		}
	}
	if len(added) == 0 {
		fmt.Printf("%s has every secret; nothing changed\n", *out)
		return nil
	}
	w, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := w.WriteString(b.String()); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	fmt.Printf("added %s to %s\n", strings.Join(added, ", "), *out)
	return nil
}
