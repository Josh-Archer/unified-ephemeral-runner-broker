package tier

import (
	"context"
	"log"
	"math/rand"
	"time"
)

type Refresher interface {
	Refresh(context.Context) ([]Decision, error)
}

func StartRefreshLoop(ctx context.Context, manager *Manager, refresher Refresher, interval time.Duration) {
	if manager == nil || refresher == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		timer := time.NewTimer(jitter(interval))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				decisions, err := refresher.Refresh(ctx)
				if err != nil {
					log.Printf("tier refresh failed: %v", err)
				}
				for _, decision := range decisions {
					manager.SetDecision(decision)
				}
				timer.Reset(jitter(interval))
			}
		}
	}()
}

func jitter(interval time.Duration) time.Duration {
	if interval <= time.Second {
		return interval
	}
	maxJitter := int64(interval / 10)
	if maxJitter <= 0 {
		return interval
	}
	return interval + time.Duration(rand.Int63n(maxJitter))
}
