package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func capsService(t *testing.T, arc model.BackendConfig) *Service {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{model.BackendARC: arc}
	return NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
}

func TestNoCapacityCapsNamesPoolMaxRunners(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 5, Template: "arc-full"})
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=maxRunners(1/1)" {
		t.Fatalf("caps = %q", got)
	}
}

func TestNoCapacityCapsNamesCrossPoolCap(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4, CrossPoolMaxRunners: 1, Template: "arc-full"})
	// Another pool's runner on the same backend fills the shared cap.
	if err := svc.store.Save(model.AllocationStatus{ID: "other", Pool: "other-pool", SelectedBackend: model.BackendARC, State: model.StateReady, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=crossPoolMaxRunners(1/1)" {
		t.Fatalf("caps = %q", got)
	}
}

func TestNoCapacityCapsReportsDisabledAndFree(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: false, MaxRunners: 2, Template: "arc-full"})
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=disabled" {
		t.Fatalf("caps = %q", got)
	}
	svc = capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full"})
	if got := svc.noCapacityCaps(model.PoolFull); !strings.HasPrefix(got, "arc=free(0/2)") {
		t.Fatalf("caps = %q", got)
	}
}
