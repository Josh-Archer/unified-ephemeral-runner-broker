package runtime

import (
	"context"
	"sync"
	"time"
)

const (
	defaultSecretCacheTTL = 30 * time.Second
	defaultSecretReadTimeout = 5 * time.Second
)

// CachingSecretReader memoizes Kubernetes secret payloads briefly and always
// reads with a detached timeout context so a cancelled parent (reconcile
// deadline, webhook request cancel) cannot surface as "context canceled"
// mid-read (uecb#201).
type CachingSecretReader struct {
	inner   SecretReader
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedSecret
}

type cachedSecret struct {
	values    map[string]string
	expiresAt time.Time
}

// NewCachingSecretReader wraps inner. A non-positive ttl or timeout falls back
// to the package defaults.
func NewCachingSecretReader(inner SecretReader, ttl, timeout time.Duration) *CachingSecretReader {
	if ttl <= 0 {
		ttl = defaultSecretCacheTTL
	}
	if timeout <= 0 {
		timeout = defaultSecretReadTimeout
	}
	return &CachingSecretReader{
		inner:   inner,
		ttl:     ttl,
		timeout: timeout,
		now:     time.Now,
		cache:   map[string]cachedSecret{},
	}
}

func (c *CachingSecretReader) ReadSecret(ctx context.Context, name string) (map[string]string, error) {
	if c == nil || c.inner == nil {
		return nil, errSecretReaderUnavailable
	}
	now := c.now()
	c.mu.Lock()
	if hit, ok := c.cache[name]; ok && now.Before(hit.expiresAt) {
		out := cloneSecretMap(hit.values)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	// Detach from parent cancelation but still bound the call.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	values, err := c.inner.ReadSecret(readCtx, name)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.cache[name] = cachedSecret{values: cloneSecretMap(values), expiresAt: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return cloneSecretMap(values), nil
}

func cloneSecretMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
