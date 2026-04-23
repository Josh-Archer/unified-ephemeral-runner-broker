package azurefunctions

import (
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/externaldispatch"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

type Backend = externaldispatch.Backend

func New(cfg model.BrokerConfig, secrets runtime.SecretReader) *Backend {
	return externaldispatch.New(model.BackendAzureFunctions, cfg, secrets)
}
