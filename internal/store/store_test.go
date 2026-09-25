package store

import (
	"context"
	"os"
	"path/filepath"
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

func TestMemorySaveIfState(t *testing.T) {
	s := NewMemory()
	_ = s.Save(model.AllocationStatus{
		ID:              "r1",
		State:           model.StateReserved,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendCodeBuild,
	})

	ready := model.AllocationStatus{
		ID:              "r1",
		State:           model.StateReady,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendCodeBuild,
		RunnerLabel:     "runner-r1",
		Metadata:        map[string]string{"execution_id": "exec-1"},
	}
	ok, err := s.SaveIfState(ready, model.StateReserved)
	if err != nil || !ok {
		t.Fatalf("expected reserved->ready save, ok=%v err=%v", ok, err)
	}
	got, found := s.Get("r1")
	if !found || got.State != model.StateReady || got.RunnerLabel != "runner-r1" {
		t.Fatalf("unexpected saved allocation: found=%v %+v", found, got)
	}

	// Cancel wins: subsequent ready commit must not overwrite.
	_, _ = s.MarkState("r1", model.StateCanceled, time.Now(), "")
	overwrite := ready
	overwrite.RunnerLabel = "orphan-label"
	ok, err = s.SaveIfState(overwrite, model.StateReserved)
	if err != nil {
		t.Fatalf("SaveIfState error: %v", err)
	}
	if ok {
		t.Fatal("expected SaveIfState to reject non-reserved state")
	}
	got, _ = s.Get("r1")
	if got.State != model.StateCanceled || got.RunnerLabel != "runner-r1" {
		t.Fatalf("canceled allocation was overwritten: %+v", got)
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

func TestFileStoreMarkStatePersistFailure(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	s, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	initial := model.AllocationStatus{
		ID:              "alloc-1",
		State:           model.StateReserved,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendARC,
	}
	if err := s.Save(initial); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Block file writes by making the .tmp path a directory
	tmpPath := filePath + ".tmp"
	if err := os.Mkdir(tmpPath, 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	// MarkState should fail and return false when persistLocked fails
	status, ok := s.MarkState("alloc-1", model.StateReady, time.Now(), "failed update")
	if ok {
		t.Fatalf("expected MarkState to fail on persist error, got ok=true, status=%+v", status)
	}

	// In-memory record should retain the previous state
	got, found := s.Get("alloc-1")
	if !found {
		t.Fatal("expected alloc-1 to still exist")
	}
	if got.State != model.StateReserved {
		t.Fatalf("expected in-memory state to remain reserved, got %s", got.State)
	}

	// On-disk record reloaded in a fresh store should also retain previous state
	reloaded, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("reload NewFile: %v", err)
	}
	reloadedStatus, found := reloaded.Get("alloc-1")
	if !found || reloadedStatus.State != model.StateReserved {
		t.Fatalf("expected on-disk state reserved, got found=%v status=%+v", found, reloadedStatus)
	}

	// Remove write blocker; subsequent MarkState should succeed
	if err := os.Remove(tmpPath); err != nil {
		t.Fatalf("remove tmp dir: %v", err)
	}
	status, ok = s.MarkState("alloc-1", model.StateReady, time.Now(), "success")
	if !ok || status.State != model.StateReady {
		t.Fatalf("expected MarkState to succeed after unblocking, got ok=%v status=%+v", ok, status)
	}
	reloaded2, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("reload2 NewFile: %v", err)
	}
	if reloadedStatus2, found := reloaded2.Get("alloc-1"); !found || reloadedStatus2.State != model.StateReady {
		t.Fatalf("expected on-disk state ready after unblocking, got found=%v status=%+v", found, reloadedStatus2)
	}
}

func TestFileStoreCompareAndMarkStatePersistFailure(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	s, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	initial := model.AllocationStatus{
		ID:              "alloc-cas-1",
		State:           model.StateReserved,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendARC,
	}
	if err := s.Save(initial); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Block file writes
	tmpPath := filePath + ".tmp"
	if err := os.Mkdir(tmpPath, 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	// CompareAndMarkState should fail and return false when persistLocked fails
	status, ok := s.CompareAndMarkState("alloc-cas-1", model.StateReserved, model.StateReady, time.Now(), "failed write")
	if ok {
		t.Fatalf("expected CompareAndMarkState to fail on persist error, got ok=true status=%+v", status)
	}

	// In-memory record should retain the previous state
	got, found := s.Get("alloc-cas-1")
	if !found {
		t.Fatal("expected alloc-cas-1 to still exist")
	}
	if got.State != model.StateReserved {
		t.Fatalf("expected in-memory state to remain reserved, got %s", got.State)
	}

	// On-disk record should also retain previous state
	reloaded, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("reload NewFile: %v", err)
	}
	reloadedStatus, found := reloaded.Get("alloc-cas-1")
	if !found || reloadedStatus.State != model.StateReserved {
		t.Fatalf("expected on-disk state reserved, got found=%v status=%+v", found, reloadedStatus)
	}

	// Remove blocker and verify successful CAS
	if err := os.Remove(tmpPath); err != nil {
		t.Fatalf("remove tmp: %v", err)
	}
	status, ok = s.CompareAndMarkState("alloc-cas-1", model.StateReserved, model.StateReady, time.Now(), "success")
	if !ok || status.State != model.StateReady {
		t.Fatalf("expected CompareAndMarkState success after unblocking, got ok=%v status=%+v", ok, status)
	}
	reloaded2, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("reload2 NewFile: %v", err)
	}
	if reloadedStatus2, found := reloaded2.Get("alloc-cas-1"); !found || reloadedStatus2.State != model.StateReady {
		t.Fatalf("expected on-disk state ready after unblocking, got found=%v status=%+v", found, reloadedStatus2)
	}
}

func TestFileStoreSaveIfStatePersistFailure(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "state.json")
	s, err := NewFile(filePath)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	initial := model.AllocationStatus{
		ID:              "alloc-saveif-1",
		State:           model.StateReserved,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendARC,
	}
	if err := s.Save(initial); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Block file writes
	tmpPath := filePath + ".tmp"
	if err := os.Mkdir(tmpPath, 0o755); err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}

	ready := initial
	ready.State = model.StateReady
	ready.RunnerLabel = "lbl-123"

	ok, err := s.SaveIfState(ready, model.StateReserved)
	if ok || err == nil {
		t.Fatalf("expected SaveIfState to fail on persist error, got ok=%v err=%v", ok, err)
	}

	// In-memory record should retain the previous state
	got, found := s.Get("alloc-saveif-1")
	if !found {
		t.Fatal("expected alloc-saveif-1 to still exist")
	}
	if got.State != model.StateReserved || got.RunnerLabel != "" {
		t.Fatalf("expected in-memory state to remain reserved without label, got %+v", got)
	}
}
