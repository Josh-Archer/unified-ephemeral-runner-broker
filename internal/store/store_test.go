package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestMemorySaveIfCapacityEnforcesMaxRunners(t *testing.T) {
	s := NewMemory()
	base := model.AllocationStatus{
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendARC,
		State:           model.StateReserved,
		Tenant:          "t1",
	}

	for i := 0; i < 2; i++ {
		status := base
		status.ID = "a" + string(rune('1'+i))
		if err := s.SaveIfCapacity(status, 2, 0); err != nil {
			t.Fatalf("unexpected error on save %d: %v", i, err)
		}
	}
	overflow := base
	overflow.ID = "overflow"
	if err := s.SaveIfCapacity(overflow, 2, 0); err != ErrNoCapacity {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
	if got := s.CountActive(model.PoolLite, model.BackendARC); got != 2 {
		t.Fatalf("expected active=2, got %d", got)
	}
}

func TestMemorySaveIfCapacityConcurrent(t *testing.T) {
	s := NewMemory()
	const maxRunners = 5
	const workers = 40

	var success atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			status := model.AllocationStatus{
				ID:              "id-" + itoa(i),
				Pool:            model.PoolLite,
				SelectedBackend: model.BackendCodeBuild,
				State:           model.StateReserved,
			}
			if err := s.SaveIfCapacity(status, maxRunners, 0); err == nil {
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if success.Load() != maxRunners {
		t.Fatalf("expected %d successful reservations, got %d", maxRunners, success.Load())
	}
	if got := s.CountActive(model.PoolLite, model.BackendCodeBuild); got != maxRunners {
		t.Fatalf("expected active=%d, got %d", maxRunners, got)
	}
}

func TestMemoryCompareAndMarkState(t *testing.T) {
	s := NewMemory()
	_ = s.Save(model.AllocationStatus{ID: "w1", State: model.StateWarm, Pool: model.PoolLite, SelectedBackend: model.BackendCodeBuild})

	claimed, ok := s.CompareAndMarkState("w1", model.StateWarm, model.StateReady, time.Now(), "")
	if !ok || claimed.State != model.StateReady {
		t.Fatalf("expected warm->ready claim, got ok=%v state=%s", ok, claimed.State)
	}
	if _, ok := s.CompareAndMarkState("w1", model.StateWarm, model.StateReady, time.Now(), ""); ok {
		t.Fatal("second warm claim should fail")
	}
}

func TestMemoryLeaderElection(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	ok, err := s.TryAcquireLeadership(ctx, LeaderLeaseName, "pod-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("pod-a should acquire leadership: ok=%v err=%v", ok, err)
	}
	ok, err = s.TryAcquireLeadership(ctx, LeaderLeaseName, "pod-b", time.Minute)
	if err != nil {
		t.Fatalf("pod-b election error: %v", err)
	}
	if ok {
		t.Fatal("pod-b should not steal active lease")
	}
	if err := s.ReleaseLeadership(ctx, LeaderLeaseName, "pod-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err = s.TryAcquireLeadership(ctx, LeaderLeaseName, "pod-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("pod-b should acquire after release: ok=%v err=%v", ok, err)
	}
}

func TestMemoryTenantQuota(t *testing.T) {
	s := NewMemory()
	for i := 0; i < 2; i++ {
		status := model.AllocationStatus{
			ID:              "t-" + itoa(i),
			Pool:            model.PoolLite,
			SelectedBackend: model.BackendARC,
			State:           model.StateReady,
			Tenant:          "acme",
		}
		if err := s.SaveIfCapacity(status, 10, 2); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	overflow := model.AllocationStatus{
		ID:              "t-overflow",
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendCodeBuild,
		State:           model.StateReserved,
		Tenant:          "acme",
	}
	if err := s.SaveIfCapacity(overflow, 10, 2); err != ErrNoCapacity {
		t.Fatalf("expected tenant quota ErrNoCapacity, got %v", err)
	}
}

func TestIsSharedAndProcessLocal(t *testing.T) {
	if !IsShared("postgres") || IsProcessLocal("postgres") {
		t.Fatal("postgres should be shared")
	}
	if IsShared("memory") || !IsProcessLocal("memory") {
		t.Fatal("memory should be process-local")
	}
	if IsShared("file") || !IsProcessLocal("file") {
		t.Fatal("file should be process-local")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
