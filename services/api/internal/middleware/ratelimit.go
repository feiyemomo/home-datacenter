package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"home-datacenter-api/internal/utils"
)

// IPLimiter is a per-IP token-bucket rate limiter. Each unique
// client IP gets its own *rate.Limiter with `burst` tokens
// replenished at `rps` per second. A background goroutine evicts
// entries that have been idle for more than `ttl` to bound memory
// in the face of IP-spoofing / botnet scan traffic.
//
// This is an in-process limiter. It is sufficient for the home
// use-case (single home-api instance). If the deployment is
// horizontally scaled, swap the storage for Redis (see
// docs/security.md §13 for the migration plan).
type IPLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipEntry
	rps      rate.Limit
	burst    int
	ttl      time.Duration
	stop     chan struct{}
}

type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewIPLimiter returns a limiter with the given rate (events per
// second) and burst. rps=0.1, burst=5 is a sensible default for
// /auth/bind (5 attempts in quick succession, then 1 per 10s).
//
// The background garbage collector is started here and stops when
// the first caller invokes Stop().
func NewIPLimiter(rps float64, burst int) *IPLimiter {
	l := &IPLimiter{
		limiters: make(map[string]*ipEntry),
		rps:      rate.Limit(rps),
		burst:    burst,
		ttl:      10 * time.Minute,
		stop:     make(chan struct{}),
	}
	go l.gc()
	return l
}

// Allow consumes a single token from the bucket for `ip` and
// reports whether the request is permitted. Touches lastSeen
// even when the entry already exists, so the GC keeps recent
// IPs alive.
func (l *IPLimiter) Allow(ip string) bool {
	l.mu.Lock()
	e, ok := l.limiters[ip]
	if !ok {
		e = &ipEntry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.limiters[ip] = e
	}
	e.lastSeen = time.Now()
	l.mu.Unlock()
	return e.limiter.Allow()
}

// Stop terminates the background GC goroutine. The limiter
// remains usable (Allow will keep working) but the map will
// only grow.
func (l *IPLimiter) Stop() {
	select {
	case <-l.stop:
		// already closed
	default:
		close(l.stop)
	}
}

// gc sweeps the limiters map every 5 minutes, removing entries
// that have not been seen for `ttl`. Without this, an attacker
// that rotates source IPs (or a botnet scan) would grow the map
// without bound.
func (l *IPLimiter) gc() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			cutoff := now.Add(-l.ttl)
			l.mu.Lock()
			for ip, e := range l.limiters {
				if e.lastSeen.Before(cutoff) {
					delete(l.limiters, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// RateLimitByIP returns a Gin middleware that throttles requests
// per c.ClientIP(). Rejected requests get a generic 429 — the
// message is deliberately identical to the "invalid credentials"
// error /auth/bind returns, so a probing attacker cannot tell
// rate limiting apart from auth failure (a useful little bit of
// noise).
func RateLimitByIP(l *IPLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !l.Allow(ip) {
			utils.Fail(c, http.StatusTooManyRequests, "invalid credentials")
			c.Abort()
			return
		}
		c.Next()
	}
}
