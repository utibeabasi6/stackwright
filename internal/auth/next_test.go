package auth

import "testing"

func TestSafeNextKeepsPathsOnThisSite(t *testing.T) {
	for next, want := range map[string]string{
		"/acme/runs":       "/acme/runs",
		"//evil.example":   "/fallback",
		"/\\evil.example":  "/fallback",
		"/\t/evil.example": "/fallback",
		"/\n/evil.example": "/fallback",
		"https://evil":     "/fallback",
		"":                 "/fallback",
	} {
		if got := safeNext(next, "/fallback"); got != want {
			t.Errorf("%q: %q, want %q", next, got, want)
		}
	}
}
