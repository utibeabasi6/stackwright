package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestLimiterSpendsTheBurstThenRefills(t *testing.T) {
	l := newLimiter(2, 3)
	now := time.Unix(0, 0)
	for i := range 3 {
		if ok, _ := l.take("a", now); !ok {
			t.Fatalf("take %d of the burst refused", i)
		}
	}
	ok, wait := l.take("a", now)
	if ok || wait != 500*time.Millisecond {
		t.Errorf("past the burst: %t, wait %s", ok, wait)
	}
	if ok, _ := l.take("b", now); !ok {
		t.Error("another key shares the bucket")
	}
	if ok, _ := l.take("a", now.Add(500*time.Millisecond)); !ok {
		t.Error("no token after the wait")
	}
}

func TestTrustedProxiesNameTheClient(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	var got string
	h := trustProxies(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	for _, tc := range []struct{ remote, xff, want string }{
		{"10.0.0.5:1234", "203.0.113.7", "203.0.113.7:0"},
		{"10.0.0.5:1234", "198.51.100.1, 203.0.113.7, 10.0.0.9", "203.0.113.7:0"},
		{"203.0.113.9:1234", "198.51.100.1", "203.0.113.9:1234"},
		{"10.0.0.5:1234", "", "10.0.0.5:1234"},
		{"10.0.0.5:1234", "not-an-ip", "10.0.0.5:1234"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != tc.want {
			t.Errorf("%s with %q: %s, want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}
