package auth

import (
	"sync"
	"time"
)

// Limiter is a fixed-window in-memory counter. It is per-process, which is
// fine while the API runs as a single instance; move to Postgres if that changes.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*bucket
	now    func() time.Time
	// OnDeny, if set, is called each time an attempt is refused (for metrics).
	OnDeny func()
}

type bucket struct {
	count int
	reset time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, hits: map[string]*bucket{}, now: time.Now}
}

// Allow records one attempt for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.hits) > 10000 { // opportunistic cleanup
		for k, b := range l.hits {
			if now.After(b.reset) {
				delete(l.hits, k)
			}
		}
	}
	b, ok := l.hits[key]
	if !ok || now.After(b.reset) {
		b = &bucket{reset: now.Add(l.window)}
		l.hits[key] = b
	}
	b.count++
	if b.count > l.max && l.OnDeny != nil {
		l.OnDeny()
	}
	return b.count <= l.max
}

// Reset clears a key, e.g. after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}
