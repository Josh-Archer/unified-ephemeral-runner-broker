package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestRepositoryBackendEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, repo                                    string
		managed, pinned, fallback, wantAzure, wantErr bool
	}{
		{"home", "Josh-Archer/home", true, false, true, true, false},
		{"case", "josh-archer/HOME", true, false, true, true, false},
		{"default", "", true, false, true, true, false},
		{"questkit", "Josh-Archer/questkit", true, false, true, false, false},
		{"pinned incompatible", "Josh-Archer/questkit", true, true, true, false, true},
		{"none eligible", "Josh-Archer/questkit", true, false, false, false, true},
		{"unmanaged unchanged", "Josh-Archer/questkit", false, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := model.BackendConfig{Enabled: true}
			if tc.managed {
				cfg.AzureVM = &model.AzureVMBackendConfig{Repository: "Josh-Archer/home"}
			}
			pool := model.PoolConfig{Backends: map[model.BackendName]model.BackendConfig{model.BackendAzureVM: cfg}}
			if tc.fallback {
				pool.Backends[model.BackendGCE] = model.BackendConfig{Enabled: true}
			}
			req := model.AllocationRequest{}
			if tc.pinned {
				b := model.BackendAzureVM
				req.Backend = &b
			}
			got, err := filterBackendsByRepository(pool, req, tc.repo)
			if tc.wantErr {
				if !errors.Is(err, ErrNoMatchingBackendRepository) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, ok := got.Backends[model.BackendAzureVM]
			if ok != tc.wantAzure {
				t.Fatalf("Azure eligible=%v", ok)
			}
			if _, ok := pool.Backends[model.BackendAzureVM]; !ok {
				t.Fatal("mutated original pool")
			}
		})
	}
}

func TestAllocateSkipsIncompatibleManagedAzure(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for n, c := range pool.Backends {
			c.Enabled = false
			pool.Backends[n] = c
		}
		az := pool.Backends[model.BackendAzureVM]
		az.Enabled = true
		az.AzureVM = &model.AzureVMBackendConfig{Repository: "Josh-Archer/home"}
		pool.Backends[model.BackendAzureVM] = az
		gce := pool.Backends[model.BackendGCE]
		gce.Enabled = true
		pool.Backends[model.BackendGCE] = gce
	})
	got, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, TargetRepository: "Josh-Archer/questkit"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SelectedBackend != model.BackendGCE {
		t.Fatalf("selected %v", got.SelectedBackend)
	}
}

func TestAllocateRepositoryEligibilityUsesEffectiveTarget(t *testing.T) {
	for _, kind := range []string{"principal", "saved retry", "explicit target"} {
		t.Run(kind, func(t *testing.T) {
			azure := &countingBackend{testBackend: testBackend{name: model.BackendAzureVM}}
			service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
				if pool.Name != model.PoolLite {
					return
				}
				for n, c := range pool.Backends {
					c.Enabled = false
					pool.Backends[n] = c
				}
				az := pool.Backends[model.BackendAzureVM]
				az.Enabled = true
				az.AzureVM = &model.AzureVMBackendConfig{Repository: "Josh-Archer/home"}
				pool.Backends[model.BackendAzureVM] = az
				gce := pool.Backends[model.BackendGCE]
				gce.Enabled = true
				pool.Backends[model.BackendGCE] = gce
			}, azure)
			req := model.AllocationRequest{Pool: model.PoolLite}
			ctx := context.Background()
			existing := model.AllocationStatus{}
			switch kind {
			case "principal":
				ctx = withPrincipal(ctx, OIDCClaims{Sub: "repo:Josh-Archer/questkit:ref:refs/heads/main", Repository: "Josh-Archer/questkit"})
			case "saved retry":
				existing.Repository = "Josh-Archer/questkit"
			case "explicit target":
				req.TargetRepository = "Josh-Archer/questkit"
				existing.Repository = "Josh-Archer/home"
			}
			got, err := service.allocateNow(ctx, req, existing)
			if err != nil {
				t.Fatal(err)
			}
			if got.SelectedBackend != model.BackendGCE || azure.provisionCount != 0 {
				t.Fatalf("backend=%v azure provisions=%d", got.SelectedBackend, azure.provisionCount)
			}
		})
	}
}

func TestPinnedIncompatibleRepositoryDoesNotProvision(t *testing.T) {
	azure := &countingBackend{testBackend: testBackend{name: model.BackendAzureVM}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		az := pool.Backends[model.BackendAzureVM]
		az.Enabled = true
		az.AzureVM = &model.AzureVMBackendConfig{Repository: "Josh-Archer/home"}
		pool.Backends[model.BackendAzureVM] = az
	}, azure)
	name := model.BackendAzureVM
	_, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, Backend: &name, TargetRepository: "Josh-Archer/questkit"})
	if !errors.Is(err, ErrNoMatchingBackendRepository) {
		t.Fatalf("error=%v", err)
	}
	if azure.provisionCount != 0 {
		t.Fatalf("provisions=%d", azure.provisionCount)
	}
	allocations := service.store.List()
	if len(allocations) != 0 {
		t.Fatalf("unexpected allocations: %d", len(allocations))
	}
}
