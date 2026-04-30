package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	azurebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurefunctions"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

type testBackend struct {
	name model.BackendName
}

type missingSecretReader struct{}

func (missingSecretReader) ReadSecret(context.Context, string) (map[string]string, error) {
	return nil, errors.New("secret not found")
}

func (b testBackend) Name() model.BackendName {
	return b.name
}

func (b testBackend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	return backend.ProvisionedRunner{
		RunnerLabel: backend.DefaultRunnerLabel(b.name, allocation.ID),
		Metadata: map[string]string{
			"pool":        string(request.Pool),
			"provisioner": fmt.Sprintf("test-%s", b.name),
		},
	}, nil
}

func newService() *Service {
	return newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})
}

func newServiceWithConfig(mutator func(*model.PoolConfig)) *Service {
	cfg := config.Default()
	for index := range cfg.Pools {
		if mutator != nil {
			mutator(&cfg.Pools[index])
		}
	}

	return NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendCodeBuild},
			testBackend{name: model.BackendLambda},
			testBackend{name: model.BackendCloudRun},
			testBackend{name: model.BackendAzureFunctions},
		),
		nil,
	)
}

func TestAllocateUsesWeightedSchedulerForPool(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		pool.Scheduler = scheduler.NameWeightedRoundRobin

		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Weight = 3
		codebuildCfg.MaxRunners = 1
		pool.Backends[model.BackendCodeBuild] = codebuildCfg

		arcCfg := pool.Backends[model.BackendARC]
		arcCfg.Weight = 1
		arcCfg.MaxRunners = 1
		pool.Backends[model.BackendARC] = arcCfg
	})

	want := []model.BackendName{
		model.BackendARC,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
	}

	for index, expected := range want {
		allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
		if err != nil {
			t.Fatalf("allocate #%d failed: %v", index+1, err)
		}
		if allocation.SelectedBackend != expected {
			t.Fatalf("allocate #%d selected %s, want %s", index+1, allocation.SelectedBackend, expected)
		}
		if _, ok := service.Cancel(allocation.ID); !ok {
			t.Fatalf("cancel #%d failed", index+1)
		}
	}
}

func TestAllocateUsesPriorityFairShareScheduler(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		pool.FairShare.Enabled = true

		arcCfg := pool.Backends[model.BackendARC]
		arcCfg.Enabled = true
		arcCfg.MaxRunners = 4
		pool.Backends[model.BackendARC] = arcCfg

		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.MaxRunners = 4
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})

	pinnedArc := model.BackendARC
	for range 2 {
		_, err := service.Allocate(context.Background(), model.AllocationRequest{
			Pool:          model.PoolLite,
			Backend:       &pinnedArc,
			Tenant:        "tenant-a",
			PriorityClass: string(model.PriorityClassHigh),
			JobTimeout:    5 * time.Minute,
		})
		if err != nil {
			t.Fatalf("tenant-a arc allocation failed: %v", err)
		}
	}

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:          model.PoolLite,
		Tenant:        "tenant-b",
		PriorityClass: string(model.PriorityClassNormal),
		JobTimeout:    5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("tenant-b normal allocation failed: %v", err)
	}
	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected tenant-b to use fair-share low-load backend, got %s", allocation.SelectedBackend)
	}
	if allocation.Tenant != "tenant-b" {
		t.Fatalf("expected tenant metadata on allocation, got %q", allocation.Tenant)
	}
	if allocation.PriorityClass != string(model.PriorityClassNormal) {
		t.Fatalf("expected priority metadata on allocation, got %q", allocation.PriorityClass)
	}

	if _, ok := service.Cancel(allocation.ID); !ok {
		t.Fatal("expected cancel to succeed")
	}
}

func TestAllocateFailsForUnknownScheduler(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolLite {
			pool.Scheduler = "not-a-real-scheduler"
		}
	})

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite}); err == nil {
		t.Fatal("expected invalid scheduler configuration to fail")
	}

	if err := service.Health(context.Background()); err == nil {
		t.Fatal("expected health check to fail for invalid scheduler configuration")
	}
}

func TestAllocateReturnsRunnerLabel(t *testing.T) {
	service := newService()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendARC {
		t.Fatalf("expected ARC backend, got %s", allocation.SelectedBackend)
	}

	if allocation.RunnerLabel == "" {
		t.Fatal("expected non-empty runner label")
	}
}

func TestAllocateRespectsPinnedBackendTimeoutLimit(t *testing.T) {
	service := newService()
	backend := model.BackendCodeBuild

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 30 * time.Minute,
	}); err == nil {
		t.Fatal("expected timeout limit validation to fail")
	}
}

func TestAllocateSkipsBackendsThatCannotSatisfyTimeout(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, cfg := range pool.Backends {
			cfg.Enabled = false
			pool.Backends[name] = cfg
		}
		lambdaCfg := pool.Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		lambdaCfg.Healthy = true
		lambdaCfg.MaxRunners = 1
		lambdaCfg.MaxJobDuration = 15 * time.Minute
		pool.Backends[model.BackendLambda] = lambdaCfg
	})

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 20 * time.Minute,
	}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("expected timeout-ineligible backend to be skipped as no capacity, got %v", err)
	}

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("expected later valid allocation to use available lambda capacity: %v", err)
	}
	if allocation.SelectedBackend != model.BackendLambda {
		t.Fatalf("expected lambda backend, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateTreatsLegacyPinnedLambdaAsCodeBuildWhenLambdaBackendIsDisabled(t *testing.T) {
	service := newService()
	backend := model.BackendLambda

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("expected legacy lambda pin to fall back to codebuild: %v", err)
	}
	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected legacy lambda pin to select codebuild, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateUsesLambdaWhenLambdaBackendIsEnabled(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		lambdaCfg := pool.Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		lambdaCfg.MaxRunners = 1
		pool.Backends[model.BackendLambda] = lambdaCfg
	})
	backend := model.BackendLambda

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("expected lambda pin to select lambda when enabled: %v", err)
	}
	if allocation.SelectedBackend != model.BackendLambda {
		t.Fatalf("expected lambda backend, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateFailsWhenExternalBackendSecretIsMissing(t *testing.T) {
	cfg := config.Default()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg
		lambdaCfg := cfg.Pools[index].Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendLambda] = lambdaCfg
		cloudCfg := cfg.Pools[index].Backends[model.BackendCloudRun]
		cloudCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendCloudRun] = cloudCfg
		azureCfg := cfg.Pools[index].Backends[model.BackendAzureFunctions]
		azureCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendAzureFunctions] = azureCfg
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			codebuildbackend.New(cfg, missingSecretReader{}),
			lambdabackend.New(cfg, missingSecretReader{}),
			cloudbackend.New(cfg, missingSecretReader{}),
			azurebackend.New(cfg, missingSecretReader{}),
		),
		nil,
	)

	for _, backend := range []model.BackendName{
		model.BackendCodeBuild,
		model.BackendLambda,
		model.BackendCloudRun,
		model.BackendAzureFunctions,
	} {
		backend := backend
		_, err := service.Allocate(context.Background(), model.AllocationRequest{
			Pool:       model.PoolLite,
			Backend:    &backend,
			JobTimeout: 5 * time.Minute,
		})
		if err == nil {
			t.Fatalf("expected %s allocation to fail", backend)
		}
		if !strings.Contains(err.Error(), "secret not found") {
			t.Fatalf("expected secret error for %s, got %v", backend, err)
		}
	}
}

func TestCancelReleasesCapacity(t *testing.T) {
	service := newService()
	backend := model.BackendARC

	first, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}

	if _, ok := service.Cancel(first.ID); !ok {
		t.Fatal("expected cancel to succeed")
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &backend,
		JobTimeout: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("expected capacity to be reusable after cancel: %v", err)
	}
}

func TestSweepExpiredMarksReadyAllocationsExpired(t *testing.T) {
	service := newService()
	service.now = func() time.Time { return time.Unix(1000, 0) }

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	expired := service.SweepExpired(time.Unix(1100, 0))
	if expired != 1 {
		t.Fatalf("expected 1 expired allocation, got %d", expired)
	}

	updated, ok := service.Get(allocation.ID)
	if !ok {
		t.Fatal("allocation disappeared after sweep")
	}

	if updated.State != model.StateExpired {
		t.Fatalf("expected expired state, got %s", updated.State)
	}
}

func TestAllocateFailsWhenHealthCheckFails(t *testing.T) {
	service := NewService(
		config.Default(),
		backend.NewRegistry(testBackend{name: model.BackendARC}),
		func(context.Context) error { return context.DeadlineExceeded },
	)

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull}); err != context.DeadlineExceeded {
		t.Fatalf("expected health check failure, got %v", err)
	}
}

func TestAllocateKeepsExistingBehaviorWhenCapabilitiesAreEmpty(t *testing.T) {
	service := newService()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"", "  "},
		ExcludedCapabilities: []string{},
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendARC {
		t.Fatalf("expected ARC backend, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateFiltersBackendsByRequiredCapabilities(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Capabilities = []string{"gpu", "region:aws-us-east-1"}
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"GPU"},
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild backend, got %s", allocation.SelectedBackend)
	}

	if got := allocation.Metadata[backend.MetadataCapabilitiesKey]; got != "gpu,region:aws-us-east-1" {
		t.Fatalf("unexpected capability metadata: %q", got)
	}
}

func TestAllocateFiltersBackendsByExcludedCapabilities(t *testing.T) {
	service := newService()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		ExcludedCapabilities: []string{"cluster-local"},
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild backend, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateReturnsClearErrorWhenNoBackendMatchesCapabilities(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, cfg := range pool.Backends {
			cfg.Capabilities = nil
			pool.Backends[name] = cfg
		}
	})

	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"gpu"},
	})
	if !errors.Is(err, ErrNoMatchingBackendCapabilities) {
		t.Fatalf("expected capability mismatch error, got %v", err)
	}
}

func TestAllocateTreatsMissingCapabilityMetadataAsEmptySet(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, cfg := range pool.Backends {
			cfg.Capabilities = nil
			pool.Backends[name] = cfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Capabilities = nil
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})

	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"region:aws-us-east-1"},
	})
	if !errors.Is(err, ErrNoMatchingBackendCapabilities) {
		t.Fatalf("expected capability mismatch error, got %v", err)
	}
}

func TestAllocateRejectsPinnedBackendThatFailsCapabilityFilter(t *testing.T) {
	service := newService()
	pinned := model.BackendARC

	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		Backend:              &pinned,
		ExcludedCapabilities: []string{"cluster-local"},
	})
	if !errors.Is(err, ErrNoMatchingBackendCapabilities) {
		t.Fatalf("expected capability mismatch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "pinned backend") {
		t.Fatalf("expected pinned backend error context, got %v", err)
	}
}

func TestAllocateEnforcesTenantQuotas(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		pool.FairShare.Enabled = true
		pool.FairShare.Quotas = map[string]int{
			"limited-tenant": 1,
		}

		arcCfg := pool.Backends[model.BackendARC]
		arcCfg.Enabled = true
		arcCfg.MaxRunners = 4
		pool.Backends[model.BackendARC] = arcCfg
	})

	// First allocation for limited-tenant should succeed
	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:   model.PoolLite,
		Tenant: "limited-tenant",
	})
	if err != nil {
		t.Fatalf("first allocate failed: %v", err)
	}

	// Second allocation for limited-tenant should be rejected
	_, err = service.Allocate(context.Background(), model.AllocationRequest{
		Pool:   model.PoolLite,
		Tenant: "limited-tenant",
	})
	if !errors.Is(err, scheduler.ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}
