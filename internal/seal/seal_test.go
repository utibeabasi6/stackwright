package seal_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/utibeabasi6/stackwright/internal/seal"
	"github.com/utibeabasi6/stackwright/internal/testenv"
)

func newKEK(t *testing.T) seal.KEK {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	k, err := seal.ParseKEK(base64.StdEncoding.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestParseKEKRefusesShortKeys(t *testing.T) {
	if _, err := seal.ParseKEK(base64.StdEncoding.EncodeToString([]byte("short"))); !errors.Is(err, seal.ErrBadKEK) {
		t.Fatalf("err = %v, want ErrBadKEK", err)
	}
}

func TestSealAndOpen(t *testing.T) {
	ctx := context.Background()
	d := testenv.New(t)
	k := newKEK(t)
	secret := []byte("client-secret-value")

	var sealed []byte
	err := pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
		var err error
		sealed, err = k.Seal(ctx, tx, "", secret)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, secret) {
		t.Fatal("the sealed value holds the plain text")
	}

	err = pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
		got, err := k.Open(ctx, tx, "", sealed)
		if err == nil && !bytes.Equal(got, secret) {
			t.Errorf("opened %q, want %q", got, secret)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
		_, err := newKEK(t).Open(ctx, tx, "", sealed)
		return err
	})
	if !errors.Is(err, seal.ErrWrongKEK) {
		t.Fatalf("another KEK opened it: %v", err)
	}
}

func TestFirstUseFromManyRequests(t *testing.T) {
	ctx := context.Background()
	d := testenv.New(t)
	k := newKEK(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
				_, err := k.Seal(ctx, tx, "", []byte("x"))
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	var n int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM data_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d data keys (err %v), want 1", n, err)
	}
}

func TestRotateWithoutAnOutage(t *testing.T) {
	ctx := context.Background()
	d := testenv.New(t)
	text := func() string {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return base64.StdEncoding.EncodeToString(b)
	}
	oldText, nextText := text(), text()
	old, _ := seal.ParseKEK(oldText)
	next, _ := seal.ParseKEK(nextText)
	during, err := next.WithPrevious(oldText)
	if err != nil {
		t.Fatal(err)
	}
	open := func(k seal.KEK, sealed []byte) error {
		return pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
			got, err := k.Open(ctx, tx, "", sealed)
			if err == nil && string(got) != "secret" {
				t.Errorf("opened %q", got)
			}
			return err
		})
	}
	rotate := func(k seal.KEK) (n int, err error) {
		err = pgx.BeginFunc(ctx, d.Owner, func(tx pgx.Tx) error {
			n, err = k.Rotate(ctx, tx)
			return err
		})
		return n, err
	}

	var sealed []byte
	if err := pgx.BeginFunc(ctx, d.App, func(tx pgx.Tx) error {
		sealed, err = old.Seal(ctx, tx, "", []byte("secret"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := open(during, sealed); err != nil {
		t.Fatalf("the server with both KEKs: %v", err)
	}
	if err := open(next, sealed); !errors.Is(err, seal.ErrWrongKEK) {
		t.Errorf("the new KEK alone before the rotation: %v", err)
	}
	if n, err := rotate(during); n != 1 || err != nil {
		t.Fatalf("rotate: %d %v", n, err)
	}
	if err := open(next, sealed); err != nil {
		t.Errorf("the new KEK alone after the rotation: %v", err)
	}
	if err := open(old, sealed); !errors.Is(err, seal.ErrWrongKEK) {
		t.Errorf("the old KEK after the rotation: %v", err)
	}
	if n, err := rotate(during); n != 0 || err != nil {
		t.Errorf("rotate again: %d %v", n, err)
	}
	if _, err := rotate(newKEK(t)); !errors.Is(err, seal.ErrWrongKEK) {
		t.Errorf("rotate to a KEK without the previous one: %v", err)
	}
}
