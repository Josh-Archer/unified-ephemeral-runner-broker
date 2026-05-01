package ec2

import (
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/externaldispatch"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

func New(cfg model.BrokerConfig, secrets runtime.SecretReader) *externaldispatch.Backend {
	return externaldispatch.New(model.BackendEC2, cfg, secrets)
}
