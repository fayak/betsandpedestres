package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type RateLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	buckets map[string]rateEntry
}

type rateEntry struct {
	count   int
	expires time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{
		window:  window,
		limit:   limit,
		buckets: make(map[string]rateEntry),
	}
}

func (rl *RateLimiter) Allow(key string) bool {
	if rl == nil {
		return true
	}
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry := rl.buckets[key]
	if now.After(entry.expires) {
		entry.count = 0
		entry.expires = now.Add(rl.window)
	}
	if entry.count >= rl.limit {
		rl.buckets[key] = entry
		return false
	}
	entry.count++
	rl.buckets[key] = entry

	if len(rl.buckets) > rl.limit*50 {
		for k, v := range rl.buckets {
			if now.After(v.expires) {
				delete(rl.buckets, k)
			}
		}
	}

	return true
}

func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
