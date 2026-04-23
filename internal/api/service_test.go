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
			azurebackend.New(),
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
		if backend == model.BackendAzureFunctions {
			if !strings.Contains(err.Error(), "not implemented yet") {
				t.Fatalf("expected stub error for %s, got %v", backend, err)
			}
			continue
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
