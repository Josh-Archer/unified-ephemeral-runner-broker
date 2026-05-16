package mock

import (
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/adaptertest"
)

func TestAdapterConformance(t *testing.T) {
	adaptertest.RunConformance(t, func(testing.TB) adapter.Adapter {
		return New(1)
	}, adaptertest.Options{})
}
