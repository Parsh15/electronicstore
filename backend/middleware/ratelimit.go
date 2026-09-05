package middleware

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Rate limiting, per client IP, in memory.
//
// Two tiers, as specified: auth endpoints are deliberately tight because they
// are the ones worth guessing at, everything else is generous enough that
// normal use — a dashboard load fires a dozen requests — never trips it.
//
// In-memory state means each instance limits independently. For a single
// backend container that is exactly right; behind several replicas the
// effective limit multiplies by the replica count, which is a knowingly
// accepted trade rather than an oversight.

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	perMin  int
	burst   int
}

func NewLimiter(perMinute, burst int) *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}, perMin: perMinute, burst: burst}
	go l.sweep()
	return l
}

func (l *Limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(float64(l.perMin)/60.0), l.burst)}
		l.buckets[key] = b
	}
	b.seen = time.Now()
	return b.lim.Allow()
}

// sweep drops buckets nobody has used for ten minutes, so the map cannot grow
// without bound on a long-running process.
func (l *Limiter) sweep() {
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		cut := time.Now().Add(-10 * time.Minute)
		l.mu.Lock()
		for k, b := range l.buckets {
			if b.seen.Before(cut) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// Middleware returns the handler wrapper.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r) // never rate-limit preflight
			return
		}
		if !l.allow(ClientIP(r)) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "Too many requests. Please wait a moment and try again.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthLimiter: 10 requests a minute per IP, small burst — sign-in, sign-up and
// password change.
func AuthLimiter() *Limiter { return NewLimiter(10, 5) }

// APILimiter: 200 requests a minute per IP for everything else.
func APILimiter() *Limiter { return NewLimiter(200, 60) }
