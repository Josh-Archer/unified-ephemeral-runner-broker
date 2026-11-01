package azurevm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	gh "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type fakeRunners struct {
	mu         sync.Mutex
	runners    []gh.Runner
	failAdd    bool
	failRemove bool
}

func (f *fakeRunners) ListRunners(context.Context, model.GitHubScope) ([]gh.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := append([]gh.Runner{}, f.runners...)
	for i := range r {
		r[i].Labels = append([]gh.RunnerLabel{}, r[i].Labels...)
	}
	return r, nil
}
func (f *fakeRunners) AddRunnerLabels(_ context.Context, _ model.GitHubScope, id int64, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.runners {
		if f.runners[i].ID == id {
			for _, l := range labels {
				f.runners[i].Labels = append(f.runners[i].Labels, gh.RunnerLabel{Name: l, Type: "custom"})
			}
		}
	}
	if f.failAdd {
		return errors.New("lost response")
	}
	return nil
}
func (f *fakeRunners) RemoveRunnerLabel(_ context.Context, _ model.GitHubScope, id int64, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRemove {
		return errors.New("unavailable")
	}
	for i := range f.runners {
		if f.runners[i].ID != id {
			continue
		}
		ls := f.runners[i].Labels[:0]
		for _, l := range f.runners[i].Labels {
			if !strings.EqualFold(l.Name, label) {
				ls = append(ls, l)
			}
		}
		f.runners[i].Labels = ls
	}
	return nil
}
func fixture() (model.BrokerConfig, *fakeRunners) {
	c := model.BrokerConfig{GitHub: model.GitHubConfig{Scope: model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "example", Repository: "project"}}, Pools: []model.PoolConfig{{Name: model.PoolFull, Labels: []string{"self-hosted", "shared-full"}, Backends: map[model.BackendName]model.BackendConfig{model.BackendAzureVM: {Enabled: true, Healthy: true, MaxRunners: 2, CrossPoolMaxRunners: 2, AzureVM: &model.AzureVMBackendConfig{Repository: "example/project", RunnerNames: []string{"worker-1", "worker-2"}}}}}}}
	f := &fakeRunners{runners: []gh.Runner{{ID: 1, Name: "worker-1", Status: "online", Labels: []gh.RunnerLabel{{Name: "self-hosted", Type: "read-only"}, {Name: "keep", Type: "custom"}}}, {ID: 2, Name: "worker-2", Status: "online"}, {ID: 3, Name: "foreign", Status: "online"}}}
	return c, f
}
func allocate(id string) model.AllocationStatus {
	return model.AllocationStatus{ID: id, Pool: model.PoolFull}
}
func TestManagedRunnerLeaseRestartAndCleanup(t *testing.T) {
	c, f := fixture()
	b := New(c, f)
	a := allocate("one")
	r, err := b.Provision(context.Background(), model.AllocationRequest{}, a)
	if err != nil {
		t.Fatal(err)
	}
	if r.RunnerLabel != "worker-1" || r.Metadata["github_runner_id"] != "1" || r.Metadata[LeaseStartedKey] == "" {
		t.Fatalf("identity: %+v", r)
	}
	// The new adapter has no process-local lease state; GitHub markers persist.
	b = New(c, f)
	r2, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate("two"))
	if err != nil || r2.RunnerLabel != "worker-2" {
		t.Fatalf("second: %+v %v", r2, err)
	}
	if _, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate("three")); !backend.IsCapacityExhausted(err) {
		t.Fatalf("must reject occupied physical workers: %v", err)
	}
	if err := b.(backend.CleanupBackend).Cleanup(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	rs, _ := f.ListRunners(context.Background(), model.GitHubScope{})
	if len(rs[0].Labels) != 2 || rs[0].Labels[1].Name != "keep" {
		t.Fatalf("cleanup changed unrelated labels: %+v", rs[0])
	}
	// A stale cleanup cannot touch a later lease of the same worker.
	if _, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate("new")); err != nil {
		t.Fatal(err)
	}
	if err := b.(backend.CleanupBackend).Cleanup(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	rs, _ = f.ListRunners(context.Background(), model.GitHubScope{})
	if !leased(rs[0]) {
		t.Fatal("old cleanup removed a new lease")
	}
}
func TestManagedExcludesBusyOfflineAndForeign(t *testing.T) {
	c, f := fixture()
	f.runners[0].Busy = true
	f.runners[1].Status = "offline"
	b := New(c, f)
	if _, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate("one")); !backend.IsCapacityExhausted(err) {
		t.Fatalf("unavailable workers admitted: %v", err)
	}
	cap, err := b.(backend.CapacityBackend).Capacity(context.Background())
	if err != nil || backend.FreeSlots(cap) != 0 {
		t.Fatalf("capacity %+v %v", cap, err)
	}
}
func TestManagedConcurrentPhysicalClaims(t *testing.T) {
	c, f := fixture()
	b := New(c, f)
	var wg sync.WaitGroup
	var mu sync.Mutex
	names := map[string]bool{}
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate(id))
			if err != nil {
				if !backend.IsCapacityExhausted(err) {
					t.Error(err)
				}
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if names[r.RunnerLabel] {
				t.Error("duplicate physical lease")
			}
			names[r.RunnerLabel] = true
		}(id)
	}
	wg.Wait()
	if len(names) != 2 {
		t.Fatalf("leased %d physical workers", len(names))
	}
}
func TestManagedLostMutationResponseCleansOnlyOwnedLabels(t *testing.T) {
	c, f := fixture()
	f.failAdd = true
	b := New(c, f)
	if _, err := b.Provision(context.Background(), model.AllocationRequest{}, allocate("one")); err == nil {
		t.Fatal("missing error")
	}
	rs, _ := f.ListRunners(context.Background(), model.GitHubScope{})
	if leased(rs[0]) || len(rs[0].Labels) != 2 {
		t.Fatalf("mutation orphan: %+v", rs[0])
	}
}
func TestManagedCleanupFailureKeepsClaimMarker(t *testing.T) {
	c, f := fixture()
	b := New(c, f)
	a := allocate("one")
	if _, err := b.Provision(context.Background(), model.AllocationRequest{}, a); err != nil {
		t.Fatal(err)
	}
	f.failRemove = true
	if err := b.(backend.CleanupBackend).Cleanup(context.Background(), a); err == nil {
		t.Fatal("missing cleanup error")
	}
	rs, _ := f.ListRunners(context.Background(), model.GitHubScope{})
	if !leased(rs[0]) {
		t.Fatal("failed cleanup removed exclusion marker")
	}
}
func TestLegacyStaticRunnerStillAvailable(t *testing.T) {
	c, _ := fixture()
	x := c.Pools[0].Backends[model.BackendAzureVM]
	x.AzureVM = nil
	x.RunnerLabel = "legacy"
	c.Pools[0].Backends[model.BackendAzureVM] = x
	r, err := New(c).Provision(context.Background(), model.AllocationRequest{}, allocate("one"))
	if err != nil || r.RunnerLabel != "legacy" {
		t.Fatalf("legacy: %+v %v", r, err)
	}
}
