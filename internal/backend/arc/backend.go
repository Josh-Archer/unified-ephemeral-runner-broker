package arc

import (
	"context"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Backend struct{}

func New() *Backend {
	return &Backend{}
}

func (b *Backend) Name() model.BackendName {
	return model.BackendARC
}

func (b *Backend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	return backend.ProvisionedRunner{
		RunnerLabel: backend.DefaultRunnerLabel(model.BackendARC, allocation.ID),
		Metadata: map[string]string{
			"pool":            string(request.Pool),
			"capability":      "full",
			"provisioner":     "arc-job",
			"lifecycle":       "ephemeral",
			"supports_docker": "true",
		},
	}, nil
}
