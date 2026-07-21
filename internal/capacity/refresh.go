package capacity

import (
	"context"
	"log"
	"math/rand"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Reporter resolves a CapacityBackend for a backend name.
type Reporter interface {
	CapacityBackend(name model.BackendName) (backend.CapacityBackend, bool)
}

// RegistryReporter adapts backend.Registry to capacity.Reporter.
type RegistryReporter struct {
	Registry *backend.Registry
}

func (r RegistryReporter) CapacityBackend(name model.BackendName) (backend.CapacityBackend, bool) {
	if r.Registry == nil {
		return nil, false
	}
	impl, ok := r.Registry.Get(name)
	if !ok {
		return nil, false
	}
	reporter, ok := impl.(backend.CapacityBackend)
	return reporter, ok
}

// Refresh polls every enabled backend that implements CapacityBackend and
// stores the result in the manager. Polling runs outside the allocation path.
func Refresh(ctx context.Context, manager *Manager, reporter Reporter, cfg model.BrokerConfig, now time.Time) {
	if manager == nil || reporter == nil {
		return
	}

	seen := map[model.BackendName]struct{}{}
	for _, pool := range cfg.Pools {
		for name, backendCfg := range pool.Backends {
			if !backendCfg.Enabled {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}

			capacityBackend, ok := reporter.CapacityBackend(name)
			if !ok || capacityBackend == nil {
				continue
			}

			status, err := capacityBackend.Capacity(ctx)
			snapshot := Snapshot{
				Backend:   name,
				UpdatedAt: now,
				Stale:     false,
			}
			if err != nil {
				snapshot.Err = err.Error()
				snapshot.Source = "error"
				// Preserve last good counters when a refresh fails so stale
				// policy can still inspect them until MarkStale ages them out.
				if previous, ok := manager.Get(name); ok && previous.Err == "" {
					snapshot.Status = previous.Status
				}
				log.Printf("live capacity refresh failed for backend %s: %v", name, err)
			} else {
				snapshot.Status = status
				snapshot.Source = "live"
			}
			manager.Set(snapshot)
		}
	}
}

// StartRefreshLoop periodically refreshes capacity outside allocation.
func StartRefreshLoop(ctx context.Context, manager *Manager, reporter Reporter, cfg model.BrokerConfig, interval time.Duration) {
	if manager == nil || reporter == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		timer := time.NewTimer(jitter(interval))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				Refresh(ctx, manager, reporter, cfg, time.Now().UTC())
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
