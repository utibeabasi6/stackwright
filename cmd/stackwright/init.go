package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

var initSecrets = []string{
	"STACKWRIGHT_POSTGRES_PASSWORD",
	"STACKWRIGHT_NATS_PASSWORD",
	"STACKWRIGHT_S3_ACCESS_KEY",
	"STACKWRIGHT_S3_SECRET_KEY",
}

func runInit(args []string) error {
	fl := flag.NewFlagSet("init", flag.ExitOnError)
	out := fl.String("o", ".env", "file to write")
	_ = fl.Parse(args)

	var b strings.Builder
	for _, k := range initSecrets {
		// NATS misreads an environment value that starts with a digit.
		fmt.Fprintf(&b, "%s=s%s\n", k, rand.Text())
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s exists and holds the keys to existing data; move it away first", *out)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}
