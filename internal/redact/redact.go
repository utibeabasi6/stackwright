// Package redact masks secret values before the executor sends or stores
// any output.
package redact

import (
	"bytes"
	"encoding/json"
	"slices"
)

const Mask = "(sensitive value)"

type Redactor struct {
	secrets [][]byte
}

// New masks each value as written and as it appears inside a JSON string.
// Empty values are ignored.
func New(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add masks more values, such as credentials the job gets later.
func (r *Redactor) Add(values ...string) {
	for _, v := range values {
		if v == "" {
			continue
		}
		// OpenTofu writes & < > as they are; Go's encoder writes &.
		for _, s := range [][]byte{[]byte(v), jsonString(v, true), jsonString(v, false)} {
			if !slices.ContainsFunc(r.secrets, func(x []byte) bool { return bytes.Equal(x, s) }) {
				r.secrets = append(r.secrets, s)
			}
		}
	}
	// A secret can hold another, such as a URL with a password in it.
	// Longest first masks the whole URL, not only the password.
	slices.SortStableFunc(r.secrets, func(a, b []byte) int { return len(b) - len(a) })
}

func jsonString(v string, escapeHTML bool) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\"\n"))[1:]
}

func (r *Redactor) Bytes(b []byte) []byte {
	for _, s := range r.secrets {
		if bytes.Contains(b, s) {
			b = bytes.ReplaceAll(b, s, []byte(Mask))
		}
	}
	return b
}

func (r *Redactor) String(s string) string { return string(r.Bytes([]byte(s))) }
