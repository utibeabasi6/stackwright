package seal

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrBadKEK    = errors.New("the KEK must be 32 bytes, base64")
	ErrWrongKEK  = errors.New("this data key was wrapped by another KEK")
	ErrMalformed = errors.New("sealed value is malformed")
)

// KEK wraps data keys. During a rotation it also opens the data keys that
// the previous KEK wrapped.
type KEK struct {
	aead     cipher.AEAD
	id       []byte
	previous *KEK
}

func ParseKEK(b64 string) (KEK, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) != 32 {
		return KEK{}, ErrBadKEK
	}
	aead, err := newAEAD(key)
	if err != nil {
		return KEK{}, err
	}
	sum := sha256.Sum256(key)
	return KEK{aead: aead, id: sum[:8]}, nil
}

// WithPrevious returns k, which also opens data keys that previous wrapped.
func (k KEK) WithPrevious(previous string) (KEK, error) {
	p, err := ParseKEK(previous)
	if err != nil {
		return KEK{}, err
	}
	k.previous = &p
	return k, nil
}

// unwrap opens a data key with whichever KEK wrapped it.
func (k KEK) unwrap(id uuid.UUID, kekID, wrapped []byte) ([]byte, error) {
	aead := k.aead
	switch {
	case bytes.Equal(kekID, k.id):
	case k.previous != nil && bytes.Equal(kekID, k.previous.id):
		aead = k.previous.aead
	default:
		return nil, ErrWrongKEK
	}
	raw, err := decrypt(aead, wrapped, id[:])
	if err != nil {
		return nil, fmt.Errorf("unwrap data key: %w", err)
	}
	return raw, nil
}

// Rotate wraps every data key that the previous KEK wrapped with k
// instead, and returns how many it changed. tx must see every org's key.
func (k KEK) Rotate(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id, kek_id, wrapped FROM data_keys WHERE kek_id <> $1 FOR UPDATE`, k.id)
	if err != nil {
		return 0, err
	}
	type key struct {
		id             uuid.UUID
		kekID, wrapped []byte
	}
	var keys []key
	for rows.Next() {
		var c key
		if err := rows.Scan(&c.id, &c.kekID, &c.wrapped); err != nil {
			return 0, err
		}
		keys = append(keys, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, c := range keys {
		raw, err := k.unwrap(c.id, c.kekID, c.wrapped)
		if err != nil {
			return 0, fmt.Errorf("data key %s: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE data_keys SET kek_id = $2, wrapped = $3 WHERE id = $1`, c.id, k.id,
			encrypt(k.aead, raw, c.id[:])); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encrypt(aead cipher.AEAD, plain, ad []byte) []byte {
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, ad)
}

func decrypt(aead cipher.AEAD, sealed, ad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrMalformed
	}
	return aead.Open(nil, sealed[:n], sealed[n:], ad)
}

// dataKey makes the key on first use. For an org key, tx needs the org setting.
func (k KEK) dataKey(ctx context.Context, tx pgx.Tx, orgID string) (uuid.UUID, cipher.AEAD, error) {
	var id uuid.UUID
	var kekID, wrapped []byte
	err := tx.QueryRow(ctx, `SELECT id, kek_id, wrapped FROM data_keys WHERE org_id IS NOT DISTINCT FROM nullif($1, '')::uuid`,
		orgID).Scan(&id, &kekID, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		id = uuid.New()
		raw := randomKey()
		wrapped = encrypt(k.aead, raw, id[:])
		tag, err := tx.Exec(ctx, `INSERT INTO data_keys (id, org_id, kek_id, wrapped) VALUES ($1, nullif($2, '')::uuid, $3, $4)
			ON CONFLICT (org_id) DO NOTHING`, id, orgID, k.id, wrapped)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("store data key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return k.dataKey(ctx, tx, orgID)
		}
		aead, err := newAEAD(raw)
		return id, aead, err
	}
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("load data key: %w", err)
	}
	raw, err := k.unwrap(id, kekID, wrapped)
	if err != nil {
		return uuid.Nil, nil, err
	}
	aead, err := newAEAD(raw)
	return id, aead, err
}

func randomKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

// An empty orgID uses the instance key. The output starts with the data key's id.
func (k KEK) Seal(ctx context.Context, tx pgx.Tx, orgID string, plain []byte) ([]byte, error) {
	id, aead, err := k.dataKey(ctx, tx, orgID)
	if err != nil {
		return nil, err
	}
	return append(id[:], encrypt(aead, plain, id[:])...), nil
}

func (k KEK) Open(ctx context.Context, tx pgx.Tx, orgID string, sealed []byte) ([]byte, error) {
	if len(sealed) < 16 {
		return nil, ErrMalformed
	}
	id, aead, err := k.dataKey(ctx, tx, orgID)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sealed[:16], id[:]) {
		return nil, ErrMalformed
	}
	return decrypt(aead, sealed[16:], id[:])
}
