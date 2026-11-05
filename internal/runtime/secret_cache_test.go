package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type countingSecretReader struct {
	calls atomic.Int64
	delay time.Duration
	err   error
	data  map[string]string
}

func (c *countingSecretReader) ReadSecret(ctx context.Context, name string) (map[string]string, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	out := map[string]string{}
	for k, v := range c.data {
		out[k] = v
	}
	return out, nil
}

func TestCachingSecretReaderCachesUntilTTL(t *testing.T) {
	inner := &countingSecretReader{data: map[string]string{"token": "a"}}
	cache := NewCachingSecretReader(inner, time.Minute, time.Second)
	now := time.Now()
	cache.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		got, err := cache.ReadSecret(context.Background(), "uecb-github-app")
		if err != nil || got["token"] != "a" {
			t.Fatalf("read %d: %v %#v", i, err, got)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", inner.calls.Load())
	}
	now = now.Add(2 * time.Minute)
	if _, err := cache.ReadSecret(context.Background(), "uecb-github-app"); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("after TTL calls = %d, want 2", inner.calls.Load())
	}
}

func TestCachingSecretReaderIgnoresParentCancel(t *testing.T) {
	inner := &countingSecretReader{
		delay: 50 * time.Millisecond,
		data:  map[string]string{"webhook_secret": "s"},
	}
	cache := NewCachingSecretReader(inner, time.Minute, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent already canceled — old code would fail immediately
	got, err := cache.ReadSecret(ctx, "uecb-azure-functions")
	if err != nil {
		t.Fatalf("detached read failed: %v", err)
	}
	if got["webhook_secret"] != "s" {
		t.Fatalf("got %#v", got)
	}
}

func TestCachingSecretReaderHonorsOwnTimeout(t *testing.T) {
	inner := &countingSecretReader{delay: 200 * time.Millisecond, data: map[string]string{"k": "v"}}
	cache := NewCachingSecretReader(inner, time.Minute, 20*time.Millisecond)
	_, err := cache.ReadSecret(context.Background(), "slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want deadline", err)
	}
}
