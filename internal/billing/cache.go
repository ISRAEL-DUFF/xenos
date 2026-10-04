package billing

import (
	"context"
	"sync"
	"time"
)

// BalanceCache wraps Balances reads with a short TTL (the plan caches for 60
// seconds). Decisions that move money use ISpend directly; the cache is for
// dashboards and warnings only.
type BalanceCache struct {
	ISpend ISpend
	TTL    time.Duration
	Now    func() time.Time

	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	bal Balances
	at  time.Time
}

func NewBalanceCache(is ISpend, ttl time.Duration) *BalanceCache {
	return &BalanceCache{ISpend: is, TTL: ttl, Now: time.Now, m: map[string]cached{}}
}

func (c *BalanceCache) Get(ctx context.Context, customerID string) (Balances, error) {
	c.mu.Lock()
	if e, ok := c.m[customerID]; ok && c.Now().Sub(e.at) < c.TTL {
		c.mu.Unlock()
		return e.bal, nil
	}
	c.mu.Unlock()
	bal, err := c.ISpend.Balances(ctx, customerID)
	if err != nil {
		return Balances{}, err
	}
	c.mu.Lock()
	c.m[customerID] = cached{bal, c.Now()}
	c.mu.Unlock()
	return bal, nil
}

// Invalidate drops a customer's entry after we move their money.
func (c *BalanceCache) Invalidate(customerID string) {
	c.mu.Lock()
	delete(c.m, customerID)
	c.mu.Unlock()
}
