package azurevm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	gh "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// runnerPageCap counts ListRunners calls cut at gh.MaxRunnerPages. Runners
// past the cap are invisible to provisioning, cleanup and capacity.
var runnerPageCap = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "uecb_github_list_runners_page_cap_total",
	Help: "Managed azure-vm ListRunners calls truncated at their page cap, by operation (provision, cleanup or capacity); runners past the cap were not seen.",
}, []string{"operation"})

func init() {
	if err := prometheus.Register(runnerPageCap); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			panic(err)
		}
	}
}

// listRunners accepts a listing truncated at its page cap: it counts and logs
// the truncation and returns the partial list instead of failing.
func (b *managedBackend) listRunners(ctx context.Context, scope model.GitHubScope, operation string) ([]gh.Runner, error) {
	runners, err := b.client.ListRunners(ctx, scope)
	if err != nil {
		if !errors.Is(err, gh.ErrPageCapReached) {
			return nil, err
		}
		runnerPageCap.WithLabelValues(operation).Inc()
		log.Printf("managed azure-vm WARNING: %s runner listing hit its page cap (%d pages); using the %d runners returned: %v", operation, gh.MaxRunnerPages, len(runners), err)
	}
	return runners, nil
}

// RunnerClient mutates only routing labels, never the persistent runner itself.
type RunnerClient interface {
	ListRunners(context.Context, model.GitHubScope) ([]gh.Runner, error)
	AddRunnerLabels(context.Context, model.GitHubScope, int64, []string) error
	RemoveRunnerLabel(context.Context, model.GitHubScope, int64, string) error
}

type managedBackend struct {
	*Backend
	client RunnerClient
	mu     sync.Mutex
}

const leasePrefix = "uecb-azurevm-"
const LeaseStartedKey = "runner_lease_started_at"

func ownershipLabel(id, label string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(label)))
	return fmt.Sprintf("uecb-owned-%s-%x", id, sum[:8])
}
func runnerLabels(r gh.Runner) map[string]gh.RunnerLabel {
	labels := map[string]gh.RunnerLabel{}
	for _, l := range r.Labels {
		labels[strings.ToLower(l.Name)] = l
	}
	return labels
}
func leased(r gh.Runner) bool {
	for _, l := range r.Labels {
		if strings.HasPrefix(strings.ToLower(l.Name), leasePrefix) {
			return true
		}
	}
	return false
}
func allowed(c *model.AzureVMBackendConfig, r gh.Runner) bool {
	for _, name := range c.RunnerNames {
		if strings.EqualFold(name, r.Name) {
			return true
		}
	}
	return false
}
func (b *managedBackend) target(pool model.PoolName, repository string) (*model.AzureVMBackendConfig, model.GitHubScope, error) {
	cfg, ok := b.backendConfig(pool)
	if !ok || cfg.AzureVM == nil || b.client == nil {
		return nil, model.GitHubScope{}, fmt.Errorf("managed azure-vm client/target is not configured")
	}
	if repository != "" && !strings.EqualFold(repository, cfg.AzureVM.Repository) {
		return nil, model.GitHubScope{}, fmt.Errorf("managed azure-vm does not serve repository %q", repository)
	}
	scope, err := b.cfg.GitHub.Scope.ForRepository(cfg.AzureVM.Repository)
	return cfg.AzureVM, scope, err
}
func (b *managedBackend) Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	target, scope, err := b.target(allocation.Pool, allocation.TargetRepository())
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	runners, err := b.listRunners(ctx, scope, "provision")
	if err != nil {
		return backend.ProvisionedRunner{}, err
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].ID < runners[j].ID })
	var poolLabels []string
	for _, pool := range b.cfg.Pools {
		if pool.Name == allocation.Pool {
			poolLabels = pool.Labels
			break
		}
	}
	for _, r := range runners {
		if !allowed(target, r) || r.Status != "online" || r.Busy || leased(r) {
			continue
		}
		present := runnerLabels(r)
		wanted := append(append(append([]string{}, poolLabels...), allocation.RequestedLabels...), request.Labels...)
		wanted = append(wanted, r.Name)
		add := []string{}
		for _, label := range wanted {
			label = strings.TrimSpace(label)
			key := strings.ToLower(label)
			if label == "" {
				continue
			}
			if _, exists := present[key]; exists {
				continue
			}
			add = append(add, label, ownershipLabel(allocation.ID, label))
			present[key] = gh.RunnerLabel{Name: label}
		}
		marker := leasePrefix + allocation.ID
		add = append(add, marker)
		started := time.Now().UTC().Truncate(time.Second)
		if err := b.client.AddRunnerLabels(ctx, scope, r.ID, add); err != nil {
			// The HTTP response may be lost after labels were applied. Ownership proofs
			// allow cleanup even before the service persisted Provision's metadata.
			_ = b.cleanupLocked(context.WithoutCancel(ctx), allocation, scope, target)
			return backend.ProvisionedRunner{}, err
		}
		return backend.ProvisionedRunner{RunnerLabel: r.Name, Metadata: map[string]string{
			"provider": string(model.BackendAzureVM), "github_runner_id": strconv.FormatInt(r.ID, 10), LeaseStartedKey: started.Format(time.RFC3339),
		}}, nil
	}
	return backend.ProvisionedRunner{}, backend.NewAllocationError(fmt.Errorf("no online idle unleased managed azure-vm runner"), backend.ErrBackendCapacityExhausted, true)
}
func (b *managedBackend) Cleanup(ctx context.Context, allocation model.AllocationStatus) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	target, scope, err := b.target(allocation.Pool, allocation.TargetRepository())
	if err != nil {
		return err
	}
	return b.cleanupLocked(ctx, allocation, scope, target)
}
func (b *managedBackend) cleanupLocked(ctx context.Context, allocation model.AllocationStatus, scope model.GitHubScope, target *model.AzureVMBackendConfig) error {
	runners, err := b.listRunners(ctx, scope, "cleanup")
	if err != nil {
		return err
	}
	marker := leasePrefix + allocation.ID
	for _, r := range runners {
		if !allowed(target, r) {
			continue
		}
		labels := runnerLabels(r)
		if _, owned := labels[strings.ToLower(marker)]; !owned {
			continue
		}
		// Each proof identifies a label added by this exact lease. This also works
		// after restart or a crash between GitHub mutation and local state save.
		var routing, proofs []string
		prefix := "uecb-owned-" + allocation.ID + "-"
		for _, l := range r.Labels {
			if strings.HasPrefix(strings.ToLower(l.Name), strings.ToLower(prefix)) {
				proofs = append(proofs, l.Name)
				continue
			}
			if l.Type == "read-only" {
				continue
			}
			if _, owns := labels[strings.ToLower(ownershipLabel(allocation.ID, l.Name))]; owns {
				routing = append(routing, l.Name)
			}
		}
		sort.Strings(routing)
		sort.Strings(proofs)
		for _, label := range append(append(routing, proofs...), marker) {
			if err := b.client.RemoveRunnerLabel(ctx, scope, r.ID, label); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *managedBackend) Capacity(ctx context.Context) (backend.CapacityStatus, error) {
	for _, pool := range b.cfg.Pools {
		cfg, ok := pool.Backends[model.BackendAzureVM]
		if !ok || cfg.AzureVM == nil {
			continue
		}
		target, scope, err := b.target(pool.Name, "")
		if err != nil {
			return backend.CapacityStatus{}, err
		}
		rs, err := b.listRunners(ctx, scope, "capacity")
		if err != nil {
			return backend.CapacityStatus{}, err
		}
		status := backend.CapacityStatus{}
		for _, r := range rs {
			if !allowed(target, r) || r.Status != "online" {
				continue
			}
			status.MaxRunners++
			if r.Busy || leased(r) {
				status.ActiveRunners++
			}
		}
		return status, nil
	}
	return backend.CapacityStatus{}, fmt.Errorf("managed azure-vm target is not configured")
}
