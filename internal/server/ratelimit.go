package server

import (
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/stackwright/stackwright/internal/auth"
	"github.com/stackwright/stackwright/internal/authz"
)

// limiter is a token bucket for each key: rate tokens a second, up to
// burst.
type limiter struct {
	rate, burst float64
	mu          sync.Mutex
	buckets     map[string]*bucket
	sweep       time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}}
}

// take spends a token of key's bucket, or says how long until one comes.
func (l *limiter) take(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.sweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.sweep = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// The limits of each kind of caller (adr/0046). Sign-in and the other
// routes that need none are the tightest. The API is limited per signed-in
// user, after authentication, and per client IP before it, which bounds
// the lookups that unknown credentials cost.
var (
	signInLimit = struct{ rate, burst float64 }{5, 30}
	apiLimit    = struct{ rate, burst float64 }{50, 300}
	apiIPLimit  = struct{ rate, burst float64 }{100, 1000}
	scimLimit   = struct{ rate, burst float64 }{20, 200}
)

func tooMany(wait time.Duration) (string, *connect.Error) {
	seconds := strconv.Itoa(int(math.Ceil(wait.Seconds())))
	return seconds, connect.NewError(connect.CodeResourceExhausted, errors.New("too many requests; try again in "+seconds+" s"))
}

// userLimit spends one request of a signed-in user's API limit.
func userLimit() func(userID string) error {
	l := newLimiter(apiLimit.rate, apiLimit.burst)
	return func(userID string) error {
		if ok, wait := l.take(userID, time.Now()); !ok {
			seconds, err := tooMany(wait)
			err.Meta().Set("Retry-After", seconds)
			return err
		}
		return nil
	}
}

// rateLimit limits by client IP: the RPCs and routes that need no sign-in,
// SCIM, and the API before its caller is known. Jobs, builders, the state
// backend, the S3 relay and webhooks are not limited here: each has its
// own token.
func rateLimit(next http.Handler) http.Handler {
	signIn, api, scim := newLimiter(signInLimit.rate, signInLimit.burst), newLimiter(apiIPLimit.rate, apiIPLimit.burst),
		newLimiter(scimLimit.rate, scimLimit.burst)
	public := map[string]bool{}
	for procedure, rule := range authz.Policy {
		public[procedure] = rule.Access == authz.Public
	}
	errs := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var l *limiter
		switch {
		case public[p], strings.HasPrefix(p, "/auth/"), strings.HasPrefix(p, "/cli/device/"):
			l = signIn
		case strings.HasPrefix(p, "/scim/"):
			l = scim
		case strings.HasPrefix(p, "/stackwright.v1.ExecutorService/"), strings.HasPrefix(p, "/stackwright.v1.ToolboxService/"):
		case strings.HasPrefix(p, "/stackwright.v1."):
			l = api
		}
		key := auth.ClientIP(r)
		// One IPv6 client usually holds a whole /64.
		if ip, err := netip.ParseAddr(key); err == nil && ip.Is6() && !ip.Is4In6() {
			key = netip.PrefixFrom(ip, 64).Masked().String()
		}
		if l != nil {
			if ok, wait := l.take(key, time.Now()); !ok {
				seconds, err := tooMany(wait)
				w.Header().Set("Retry-After", seconds)
				if strings.HasPrefix(p, "/stackwright.v1.") && errs.IsSupported(r) {
					_ = errs.Write(w, r, err)
					return
				}
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// trustProxies sets a request's RemoteAddr to its client's, from
// X-Forwarded-For, when it comes from one of the trusted proxies. It takes
// the rightmost address that is not a trusted proxy, since a client can
// put anything on the left.
func trustProxies(trusted []netip.Prefix, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	isTrusted := func(s string) bool {
		ip, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return false
		}
		for _, p := range trusted {
			if p.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil && isTrusted(host) {
			hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			for i := len(hops) - 1; i >= 0; i-- {
				hop := strings.TrimSpace(hops[i])
				if _, err := netip.ParseAddr(hop); err != nil {
					break
				}
				if !isTrusted(hop) {
					r = r.Clone(r.Context())
					r.RemoteAddr = net.JoinHostPort(hop, "0")
					break
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
