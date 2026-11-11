// Package artifact seals the files a job reads and writes in S3, with a key
// that only the control plane and the job's sandbox hold.
package artifact

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

var ErrMalformed = errors.New("the artifact is malformed, or was sealed for another job or name")

func NewKey() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}

// Seal binds the blob to the job that wrote it and to its name, so one blob
// cannot stand in for another.
func Seal(key []byte, jobID, name string, plain []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return aead.Seal(nonce, nonce, plain, ad(jobID, name)), nil
}

func Open(key []byte, jobID, name string, sealed []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	n := aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrMalformed
	}
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], ad(jobID, name))
	if err != nil {
		return nil, ErrMalformed
	}
	return plain, nil
}

func ad(jobID, name string) []byte {
	return []byte("stackwright/artifact/v1\x00" + jobID + "\x00" + name)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
