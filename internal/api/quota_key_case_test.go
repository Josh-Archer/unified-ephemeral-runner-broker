package api

import (
	"errors"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

// Regression for #151: the transactional store-level quota passed to
// SaveIfCapacity must resolve mixed-case tenant quota keys, independent of the
// in-memory scheduler's own quota check.
func TestSaveReservedAllocationStoreQuotaMatchesMixedCaseTenantKey(t *testing.T) {
	svc := newServiceWithConfig(nil)
	pool := model.PoolConfig{
		Name: model.PoolLite,
		Backends: map[model.BackendName]model.BackendConfig{
			model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 10},
		},
		FairShare: model.FairShareConfig{
			Enabled: true,
			Quotas:  map[string]int{"TeamA": 1},
		},
	}
	alloc := func(id string) model.AllocationStatus {
		return model.AllocationStatus{
			ID:              id,
			Pool:            model.PoolLite,
			SelectedBackend: model.BackendARC,
			State:           model.StateReserved,
			Tenant:          "teama",
		}
	}
	if err := svc.saveReservedAllocation(alloc("q-1"), pool); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := svc.saveReservedAllocation(alloc("q-2"), pool); !errors.Is(err, store.ErrNoCapacity) {
		t.Fatalf("second save with quota key %q for tenant %q: got %v, want store.ErrNoCapacity", "TeamA", "teama", err)
	}
}
