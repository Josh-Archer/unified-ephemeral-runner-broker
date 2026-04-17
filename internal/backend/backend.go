package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type ProvisionedRunner struct {
	RunnerLabel string
	Metadata    map[string]string
}

type Backend interface {
	Name() model.BackendName
	Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (ProvisionedRunner, error)
}

type Registry struct {
	backends map[model.BackendName]Backend
}

func NewRegistry(entries ...Backend) *Registry {
	backends := make(map[model.BackendName]Backend, len(entries))
	for _, entry := range entries {
		backends[entry.Name()] = entry
	}
	return &Registry{backends: backends}
}

func (r *Registry) Get(name model.BackendName) (Backend, bool) {
	backend, ok := r.backends[name]
	return backend, ok
}

func DefaultRunnerLabel(name model.BackendName, allocationID string) string {
	sanitized := strings.ReplaceAll(string(name), "-", "")
	return fmt.Sprintf("uecb-%s-%s", sanitized, allocationID)
}
