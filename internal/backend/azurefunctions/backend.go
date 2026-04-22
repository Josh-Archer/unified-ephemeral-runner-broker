package azurefunctions

import (
	"context"
	"fmt"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Backend struct{}

func New() *Backend {
	return &Backend{}
}

func (b *Backend) Name() model.BackendName {
	return model.BackendAzureFunctions
}

func (b *Backend) Provision(_ context.Context, _ model.AllocationRequest, _ model.AllocationStatus) (backend.ProvisionedRunner, error) {
	return backend.ProvisionedRunner{}, fmt.Errorf("azure-functions backend is not implemented yet")
}
