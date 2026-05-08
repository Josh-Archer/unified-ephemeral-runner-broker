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
	azurevmbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurevm"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	ec2backend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/ec2"
	gcebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/gce"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

type testBackend struct {
	name model.BackendName
}

type failingBackend struct {
	testBackend
	err error
}

type probeBackend struct {
	testBackend
	errs []error
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

func (b failingBackend) Provision(context.Context, model.AllocationRequest, model.AllocationStatus) (backend.ProvisionedRunner, error) {
	return backend.ProvisionedRunner{}, b.err
}

func (b *probeBackend) Probe(context.Context, model.PoolConfig, model.BackendConfig) error {
	if len(b.errs) == 0 {
		return nil
	}
	err := b.errs[0]
	b.errs = b.errs[1:]
	return err
}

type testCleanupBackend struct {
	testBackend
	failCleanup bool
	cleanupSeen *bool
}

func (b *testCleanupBackend) Cleanup(_ context.Context, _ model.AllocationStatus) error {
	if b.cleanupSeen != nil {
		*b.cleanupSeen = true
	}
	if !b.failCleanup {
		return nil
	}
	return errors.New("cleanup failed")
}

type countingBackend struct {
	testBackend
	provisionCount int
	lastAllocation model.AllocationStatus
}

func (b *countingBackend) Provision(_ context.Context, _ model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	b.provisionCount++
	b.lastAllocation = allocation
	return backend.ProvisionedRunner{
		RunnerLabel: backend.DefaultRunnerLabel(b.name, allocation.ID),
		Metadata: map[string]string{
			"provisioned_by": string(b.name),
		},
	}, nil
}

func newServiceWithCountingBackend(mutator func(*model.PoolConfig), counting *countingBackend) *Service {
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
			counting,
			testBackend{name: model.BackendLambda},
			testBackend{name: model.BackendCloudRun},
			testBackend{name: model.BackendAzureFunctions},
			testBackend{name: model.BackendAzureVM},
			testBackend{name: model.BackendEC2},
			testBackend{name: model.BackendGCE},
		),
		nil,
	)
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
			testBackend{name: model.BackendAzureVM},
			testBackend{name: model.BackendEC2},
			testBackend{name: model.BackendGCE},
		),
		nil,
	)
}

func newServiceWithBrokerConfig(mutator func(*model.BrokerConfig)) *Service {
	cfg := config.Default()
	if mutator != nil {
		mutator(&cfg)
	}
	return NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendCodeBuild},
			testBackend{name: model.BackendLambda},
			testBackend{name: model.BackendCloudRun},
			testBackend{name: model.BackendAzureFunctions},
			testBackend{name: model.BackendAzureVM},
			testBackend{name: model.BackendEC2},
			testBackend{name: model.BackendGCE},
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

func TestAllocateSkipsTierExceededBackend(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
		for index := range cfg.Pools {
			if cfg.Pools[index].Name != model.PoolLite {
				continue
			}
			codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
			codebuildCfg.Enabled = true
			codebuildCfg.MaxRunners = 1
			cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg
		}
	})
	manager := tier.NewManager()
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendARC, State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendCodeBuild, State: tier.StateHealthy, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	service.SetTierManager(manager)

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}
	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild after tier filtering, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateUsesDeprioritizedBackendOnlyAfterHealthyBackends(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
		for index := range cfg.Pools {
			if cfg.Pools[index].Name != model.PoolLite {
				continue
			}
			codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
			codebuildCfg.Enabled = true
			codebuildCfg.MaxRunners = 1
			cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg

			arcCfg := cfg.Pools[index].Backends[model.BackendARC]
			arcCfg.MaxRunners = 1
			cfg.Pools[index].Backends[model.BackendARC] = arcCfg
		}
	})
	manager := tier.NewManager()
	now := time.Now()
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendARC, State: tier.StateApproaching, Action: tier.ActionDeprioritize, UpdatedAt: now})
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendCodeBuild, State: tier.StateHealthy, Action: tier.ActionDisable, UpdatedAt: now})
	service.SetTierManager(manager)

	first, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("first allocate failed: %v", err)
	}
	if first.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected healthy codebuild before deprioritized arc, got %s", first.SelectedBackend)
	}

	second, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("second allocate failed: %v", err)
	}
	if second.SelectedBackend != model.BackendARC {
		t.Fatalf("expected deprioritized arc after healthy capacity is full, got %s", second.SelectedBackend)
	}
}

func TestAllocateRejectsPinnedTierBlockedBackend(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
	})
	manager := tier.NewManager()
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendARC, State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	service.SetTierManager(manager)

	pinned := model.BackendARC
	_, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, Backend: &pinned})
	if !errors.Is(err, ErrBackendTierBlocked) {
		t.Fatalf("expected tier blocked error, got %v", err)
	}
}

func TestPinnedHealthyTierBackendStillReturnsCapacityErrorWhenFull(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
		for index := range cfg.Pools {
			if cfg.Pools[index].Name != model.PoolLite {
				continue
			}
			arcCfg := cfg.Pools[index].Backends[model.BackendARC]
			arcCfg.MaxRunners = 1
			cfg.Pools[index].Backends[model.BackendARC] = arcCfg
		}
	})
	manager := tier.NewManager()
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendARC, State: tier.StateHealthy, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	service.SetTierManager(manager)

	pinned := model.BackendARC
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, Backend: &pinned}); err != nil {
		t.Fatalf("first pinned allocation failed: %v", err)
	}
	_, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite, Backend: &pinned})
	if !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("expected capacity error for full pinned backend, got %v", err)
	}
}

func TestAllocatePassesThroughUnknownTierStateByDefault(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
	})
	service.SetTierManager(tier.NewManager())

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}
	if allocation.SelectedBackend == "" {
		t.Fatal("expected allocation to pass through unknown tier state")
	}
}

func TestAllocateUsesTierFallbackBackends(t *testing.T) {
	service := newServiceWithBrokerConfig(func(cfg *model.BrokerConfig) {
		cfg.Broker.TierRouting.Enabled = true
		cfg.Broker.TierRouting.FailureMode = tier.FailureModeFallback
		cfg.Broker.TierRouting.FallbackBackends = []model.BackendName{model.BackendARC}
		for index := range cfg.Pools {
			if cfg.Pools[index].Name != model.PoolLite {
				continue
			}
			codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
			codebuildCfg.Enabled = true
			codebuildCfg.MaxRunners = 1
			cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg
		}
	})
	manager := tier.NewManager()
	for _, backendName := range []model.BackendName{model.BackendARC, model.BackendCodeBuild} {
		manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: backendName, State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	}
	service.SetTierManager(manager)

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}
	if allocation.SelectedBackend != model.BackendARC {
		t.Fatalf("expected arc fallback, got %s", allocation.SelectedBackend)
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

func TestAllocateOpensCircuitForClassifiedBackendFailureAndFallsBack(t *testing.T) {
	cfg := config.Default()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		for name, backendCfg := range cfg.Pools[index].Backends {
			backendCfg.Enabled = false
			cfg.Pools[index].Backends[name] = backendCfg
		}
		codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.CircuitBreaker.Enabled = true
		codebuildCfg.CircuitBreaker.FailureThreshold = 1
		cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg

		lambdaCfg := cfg.Pools[index].Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		lambdaCfg.Healthy = true
		lambdaCfg.MaxRunners = 1
		cfg.Pools[index].Backends[model.BackendLambda] = lambdaCfg
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			failingBackend{
				testBackend: testBackend{name: model.BackendCodeBuild},
				err:         backend.NewClassifiedError(backend.FailureReasonTimeout, context.DeadlineExceeded),
			},
			testBackend{name: model.BackendLambda},
		),
		nil,
	)
	service.now = func() time.Time { return time.Unix(1000, 0) }

	pinned := model.BackendCodeBuild
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected classified timeout failure, got %v", err)
	}

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("expected fallback allocation to succeed: %v", err)
	}
	if allocation.SelectedBackend != model.BackendLambda {
		t.Fatalf("expected circuit-open codebuild to be skipped in favor of lambda, got %s", allocation.SelectedBackend)
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	}); !errors.Is(err, ErrBackendCircuitOpen) {
		t.Fatalf("expected pinned open backend to fail fast, got %v", err)
	}
}

func TestCompleteFailureClassOpensCircuit(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.CircuitBreaker.Enabled = true
		codebuildCfg.CircuitBreaker.FailureThreshold = 1
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})
	pinned := model.BackendCodeBuild
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	_, ok, err := service.Complete(context.Background(), allocation.ID, completionRequest{
		State:        "failed",
		Error:        "runner timed out waiting for job",
		FailureClass: backend.FailureReasonWaitTimeout,
	})
	if err != nil || !ok {
		t.Fatalf("completion failed: ok=%v err=%v", ok, err)
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	}); !errors.Is(err, ErrBackendCircuitOpen) {
		t.Fatalf("expected pinned open backend to fail fast, got %v", err)
	}
}

func TestRateLimitAppliesToSelectedColdBackendOnly(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.RateLimit.Enabled = true
		codebuildCfg.RateLimit.Permits = 1
		codebuildCfg.RateLimit.Interval = time.Minute
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})
	service.now = func() time.Time { return time.Unix(1000, 0) }

	pinned := model.BackendCodeBuild
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}
	if _, ok := service.Cancel(allocation.ID); !ok {
		t.Fatal("cancel failed")
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	}); !errors.Is(err, ErrBackendRateLimited) {
		t.Fatalf("expected second allocation to be rate limited, got %v", err)
	}
}

func TestHalfOpenAdmissionDoesNotConsumeProbeSlotDuringFiltering(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.CircuitBreaker.Enabled = true
		codebuildCfg.CircuitBreaker.FailureThreshold = 1
		codebuildCfg.CircuitBreaker.OpenDuration = time.Minute
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})
	now := time.Unix(1000, 0)
	service.now = func() time.Time { return now }
	pinned := model.BackendCodeBuild

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("initial allocation failed: %v", err)
	}
	_, _, err = service.Complete(context.Background(), allocation.ID, completionRequest{
		State:        "failed",
		FailureClass: backend.FailureReasonWaitTimeout,
	})
	if err != nil {
		t.Fatalf("completion failed: %v", err)
	}

	now = now.Add(2 * time.Minute)
	recovered, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("expected half-open allocation to be admitted, got %v", err)
	}
	if recovered.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild, got %s", recovered.SelectedBackend)
	}
}

func TestProbeRecoveryRequiresConsecutiveSuccesses(t *testing.T) {
	probe := &probeBackend{
		testBackend: testBackend{name: model.BackendCodeBuild},
		errs: []error{
			nil,
			backend.NewClassifiedError(backend.FailureReasonProbeFailed, context.DeadlineExceeded),
			nil,
			nil,
		},
	}
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.CircuitBreaker.Enabled = true
		codebuildCfg.CircuitBreaker.FailureThreshold = 1
		codebuildCfg.CircuitBreaker.OpenDuration = time.Second
		codebuildCfg.CircuitBreaker.ProbeInterval = time.Second
		codebuildCfg.CircuitBreaker.RecoverySuccessThreshold = 2
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})
	service.registry = backend.NewRegistry(probe)
	now := time.Unix(1000, 0)
	service.now = func() time.Time { return now }
	pinned := model.BackendCodeBuild

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("initial allocation failed: %v", err)
	}
	_, _, err = service.Complete(context.Background(), allocation.ID, completionRequest{
		State:        "failed",
		FailureClass: backend.FailureReasonWaitTimeout,
	})
	if err != nil {
		t.Fatalf("completion failed: %v", err)
	}

	for i := 0; i < 3; i++ {
		now = now.Add(2 * time.Second)
		service.ReconcileBackendHealth()
		if _, err := service.Allocate(context.Background(), model.AllocationRequest{
			Pool:       model.PoolLite,
			Backend:    &pinned,
			JobTimeout: 5 * time.Minute,
		}); !errors.Is(err, ErrBackendCircuitOpen) {
			t.Fatalf("expected circuit to remain open after probe step %d, got %v", i+1, err)
		}
	}

	now = now.Add(2 * time.Second)
	service.ReconcileBackendHealth()
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &pinned,
		JobTimeout: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("expected circuit to close after consecutive probe successes, got %v", err)
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

func TestReconcileWarmPoolsCreatesWarmAllocation(t *testing.T) {
	counting := &countingBackend{testBackend: testBackend{name: model.BackendCodeBuild}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 2
		codebuildCfg.WarmMin = 1
		codebuildCfg.WarmMax = 1
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	}, counting)

	service.ReconcileWarmPools()

	statuses := service.store.List()
	if len(statuses) != 1 {
		t.Fatalf("expected one warm allocation, got %d", len(statuses))
	}
	warm := statuses[0]
	if warm.State != model.StateWarm {
		t.Fatalf("expected warm state, got %s", warm.State)
	}
	if got := warm.Metadata[backend.MetadataLaunchModeKey]; got != launchModeWarm {
		t.Fatalf("expected launch mode %q, got %q", launchModeWarm, got)
	}
	if counting.provisionCount != 1 {
		t.Fatalf("expected one warm provisioning call, got %d", counting.provisionCount)
	}
}

func TestReconcileWarmPoolsSkipsTierBlockedBackend(t *testing.T) {
	counting := &countingBackend{testBackend: testBackend{name: model.BackendCodeBuild}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 2
		codebuildCfg.WarmMin = 1
		codebuildCfg.WarmMax = 1
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	}, counting)
	service.cfg.Broker.TierRouting.Enabled = true
	manager := tier.NewManager()
	manager.SetDecision(tier.Decision{Pool: model.PoolLite, Backend: model.BackendCodeBuild, State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: time.Now()})
	service.SetTierManager(manager)

	service.ReconcileWarmPools()

	if counting.provisionCount != 0 {
		t.Fatalf("expected tier-blocked backend not to prewarm, got %d provisions", counting.provisionCount)
	}
	if statuses := service.store.List(); len(statuses) != 0 {
		t.Fatalf("expected no warm allocations, got %+v", statuses)
	}
}

func TestAllocatePrefersWarmAllocationOverColdLaunch(t *testing.T) {
	now := time.Unix(1000, 0)
	counting := &countingBackend{testBackend: testBackend{name: model.BackendCodeBuild}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 2
		codebuildCfg.WarmMin = 1
		codebuildCfg.WarmMax = 1
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	}, counting)
	service.now = func() time.Time { return now }

	service.ReconcileWarmPools()

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocation failed: %v", err)
	}
	if allocation.Metadata[backend.MetadataLaunchModeKey] != launchModeWarm {
		t.Fatalf("expected warm launch mode, got %q", allocation.Metadata[backend.MetadataLaunchModeKey])
	}

	secondAllocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("follow-up allocation failed: %v", err)
	}
	if secondAllocation.Metadata[backend.MetadataLaunchModeKey] != launchModeCold {
		t.Fatalf("expected fallback cold launch mode, got %q", secondAllocation.Metadata[backend.MetadataLaunchModeKey])
	}
	if counting.provisionCount != 2 {
		t.Fatalf("expected one warm then one cold provision call, got %d", counting.provisionCount)
	}

	var warmCount int
	for _, status := range service.store.List() {
		if status.State == model.StateWarm {
			warmCount++
		}
	}
	if warmCount != 0 {
		t.Fatalf("expected warm allocation to be consumed, got %d", warmCount)
	}

	if _, ok := service.Get(secondAllocation.ID); !ok {
		t.Fatal("expected second allocation to be stored")
	}
}

func TestReconcileWarmPoolsRecyclesExpiredWarmAllocation(t *testing.T) {
	now := time.Unix(1000, 0)
	counting := &countingBackend{testBackend: testBackend{name: model.BackendCodeBuild}}
	service := newServiceWithCountingBackend(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolLite {
			return
		}
		for name, backendCfg := range pool.Backends {
			backendCfg.Enabled = false
			pool.Backends[name] = backendCfg
		}
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Healthy = true
		codebuildCfg.MaxRunners = 1
		codebuildCfg.WarmMin = 1
		codebuildCfg.WarmMax = 1
		codebuildCfg.WarmTTL = time.Minute
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	}, counting)
	service.now = func() time.Time { return now }

	service.ReconcileWarmPools()
	if counting.provisionCount != 1 {
		t.Fatalf("expected one warm provisioning call, got %d", counting.provisionCount)
	}

	now = now.Add(2 * time.Minute)
	service.ReconcileWarmPools()

	warmCount := 0
	for _, status := range service.store.List() {
		if status.State == model.StateWarm {
			warmCount++
		}
	}
	if warmCount != 1 {
		t.Fatalf("expected one active warm allocation after recycle/recreate, got %d", warmCount)
	}
	if counting.provisionCount != 2 {
		t.Fatalf("expected expired warm to be recreated, provision call count=%d", counting.provisionCount)
	}
	failedCount := 0
	for _, status := range service.store.List() {
		if status.State == model.StateFailed {
			failedCount++
		}
	}
	if failedCount == 0 {
		t.Fatalf("expected at least one failed allocation from recycled warm allocation")
	}
	if _, ok := service.Get(service.store.List()[0].ID); !ok {
		t.Fatal("expected active warm allocation to be stored")
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
		ec2Cfg := cfg.Pools[index].Backends[model.BackendEC2]
		ec2Cfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendEC2] = ec2Cfg
		gceCfg := cfg.Pools[index].Backends[model.BackendGCE]
		gceCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendGCE] = gceCfg
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			codebuildbackend.New(cfg, missingSecretReader{}),
			lambdabackend.New(cfg, missingSecretReader{}),
			cloudbackend.New(cfg, missingSecretReader{}),
			azurebackend.New(cfg, missingSecretReader{}),
			azurevmbackend.New(cfg),
			ec2backend.New(cfg, missingSecretReader{}),
			gcebackend.New(cfg, missingSecretReader{}),
		),
		nil,
	)

	for _, backend := range []model.BackendName{
		model.BackendCodeBuild,
		model.BackendLambda,
		model.BackendCloudRun,
		model.BackendAzureFunctions,
		model.BackendEC2,
		model.BackendGCE,
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

func TestCompleteMarksAllocationCompletedAndReleasesCapacity(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolFull {
			return
		}
		arcCfg := pool.Backends[model.BackendARC]
		arcCfg.MaxRunners = 1
		pool.Backends[model.BackendARC] = arcCfg
	})
	service.now = func() time.Time { return time.Unix(1000, 0) }

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	completed, ok, err := service.Complete(context.Background(), allocation.ID, completionRequest{State: "completed"})
	if err != nil {
		t.Fatalf("first completion failed: %v", err)
	}
	if !ok {
		t.Fatal("expected first completion to succeed")
	}
	if completed.State != model.StateCompleted {
		t.Fatalf("expected completed state, got %s", completed.State)
	}

	// Duplicate completion must remain idempotent and not break capacity accounting.
	duplicate, ok, err := service.Complete(context.Background(), allocation.ID, completionRequest{State: "completed"})
	if err != nil {
		t.Fatalf("duplicate completion failed: %v", err)
	}
	if !ok {
		t.Fatal("expected duplicate completion to return true")
	}
	if duplicate.State != model.StateCompleted {
		t.Fatalf("expected duplicate completion state to remain completed, got %s", duplicate.State)
	}

	if _, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	}); err != nil {
		t.Fatalf("expected capacity to be reusable after completion callback: %v", err)
	}
}

func TestCompleteRejectsUnknownState(t *testing.T) {
	service := newService()
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	_, _, completeErr := service.Complete(context.Background(), allocation.ID, completionRequest{State: "not-a-state"})
	if completeErr == nil {
		t.Fatal("expected unknown completion state to fail")
	}
	if !errors.Is(completeErr, ErrInvalidCompletionState) {
		t.Fatalf("expected ErrInvalidCompletionState, got: %v", completeErr)
	}
}

func TestCompleteReturnsTerminalErrorWhenAlreadyTerminal(t *testing.T) {
	service := newService()
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	failed, _, err := service.Complete(context.Background(), allocation.ID, completionRequest{State: "failed"})
	if err != nil {
		t.Fatalf("failed completion failed: %v", err)
	}
	if failed.State != model.StateFailed {
		t.Fatalf("expected failed state, got %s", failed.State)
	}

	if _, _, err = service.Complete(context.Background(), allocation.ID, completionRequest{State: "completed"}); err == nil || !errors.Is(err, ErrAllocationAlreadyCompleted) {
		t.Fatalf("expected terminal state completion error, got %v", err)
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

func TestSweepExpiredMarksReadyAllocationsQuarantinedWhenEnabled(t *testing.T) {
	cfg := config.Default()
	cfg.Broker.OrphanCleanup.Enabled = true
	cfg.Broker.OrphanCleanup.QuarantineTTL = 10 * time.Second

	service := NewService(
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
	service.now = func() time.Time { return time.Unix(1000, 0) }

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	swept := service.SweepExpired(time.Unix(1015, 0))
	if swept != 1 {
		t.Fatalf("expected 1 allocation to be swept, got %d", swept)
	}

	updated, ok := service.Get(allocation.ID)
	if !ok {
		t.Fatal("allocation disappeared after sweep")
	}
	if updated.State != model.StateQuarantined {
		t.Fatalf("expected quarantined state, got %s", updated.State)
	}
	expectedExpiry := time.Unix(1015, 0).Add(10 * time.Second)
	if !updated.ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf("expected quarantine expiry %s, got %s", expectedExpiry, updated.ExpiresAt)
	}

	service.SweepExpired(time.Unix(1025, 0))
	if updated, ok = service.Get(allocation.ID); !ok {
		t.Fatal("allocation disappeared after quarantine expiry")
	}
	if updated.State != model.StateExpired {
		t.Fatalf("expected expiry to clear quarantine, got %s", updated.State)
	}
}

func TestCompleteReturnsNotFoundForMissingAllocation(t *testing.T) {
	restartService := newService()
	service := newService()
	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}
	if _, ok, err := restartService.Complete(context.Background(), allocation.ID, completionRequest{State: "completed"}); err != nil || ok {
		t.Fatalf("expected missing allocation on restart-like service, got ok=%v err=%v", ok, err)
	}
}

func TestCompleteHandlesCleanupFailureWithoutBlocking(t *testing.T) {
	cfg := config.Default()
	arcCfg := cfg.Pools[0].Backends[model.BackendARC]
	arcCfg.MaxRunners = 1
	cfg.Pools[0].Backends[model.BackendARC] = arcCfg
	cleanupCalled := false

	service := NewService(
		cfg,
		backend.NewRegistry(
			&testCleanupBackend{
				testBackend: testBackend{name: model.BackendARC},
				failCleanup: true,
				cleanupSeen: &cleanupCalled,
			},
		),
		nil,
	)

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolFull,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	updated, ok, err := service.Complete(context.Background(), allocation.ID, completionRequest{State: "failed", Error: "runner crashed"})
	if err != nil {
		t.Fatalf("completion failed: %v", err)
	}
	if !ok {
		t.Fatal("expected completion to return true")
	}
	if updated.State != model.StateFailed {
		t.Fatalf("expected failed state, got %s", updated.State)
	}
	if !cleanupCalled {
		t.Fatal("expected cleanup hook to run even with failures")
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

func TestAllocateRoutesDockerCapabilityToDockerBackend(t *testing.T) {
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
		lambdaCfg.Capabilities = []string{"region:aws-us-east-1"}
		pool.Backends[model.BackendLambda] = lambdaCfg
		codebuildCfg := pool.Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.Capabilities = []string{"docker", "region:aws-us-east-1"}
		pool.Backends[model.BackendCodeBuild] = codebuildCfg
	})

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"docker"},
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected docker-capable codebuild backend, got %s", allocation.SelectedBackend)
	}
}

func TestAllocateAzureVMReturnsConfiguredRunnerLabel(t *testing.T) {
	cfg := config.Default()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		for name, backendCfg := range cfg.Pools[index].Backends {
			backendCfg.Enabled = false
			cfg.Pools[index].Backends[name] = backendCfg
		}
		azureVMCfg := cfg.Pools[index].Backends[model.BackendAzureVM]
		azureVMCfg.Enabled = true
		azureVMCfg.RunnerLabel = "replace-with-private-azure-vm-runner-label"
		cfg.Pools[index].Backends[model.BackendAzureVM] = azureVMCfg
	}
	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			azurevmbackend.New(cfg),
		),
		nil,
	)

	allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:                 model.PoolLite,
		RequiredCapabilities: []string{"docker", "vm", "cloud:azure"},
		JobTimeout:           30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("allocate failed: %v", err)
	}

	if allocation.SelectedBackend != model.BackendAzureVM {
		t.Fatalf("expected azure-vm backend, got %s", allocation.SelectedBackend)
	}
	if allocation.RunnerLabel != "replace-with-private-azure-vm-runner-label" {
		t.Fatalf("expected configured Azure VM runner label, got %q", allocation.RunnerLabel)
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
