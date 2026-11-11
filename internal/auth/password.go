package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var ErrWeakPassword = errors.New("a password needs 12 or more characters")

const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

func CheckStrength(password string) error {
	if utf8.RuneCountInString(password) < 12 {
		return ErrWeakPassword
	}
	return nil
}

func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// dummyHash makes a sign-in for an unknown email take as long as one for a
// known email.
var dummyHash = HashPassword("not a real password")

func PasswordMatches(encoded, password string) bool {
	if encoded == "" {
		encoded = dummyHash
		password += "\x00"
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}
