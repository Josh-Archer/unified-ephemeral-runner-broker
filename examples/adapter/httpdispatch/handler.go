// Package httpdispatch wires the mock v1 adapter to the external-dispatch
// HTTP contract. It is the reference shape for a third-party controller.
package httpdispatch

import (
	"net/http"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/examples/adapter/mock"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch"
)

// New serves dispatch, cleanup, health, and capacity for the mock adapter.
// maxRunners is the fixture ceiling. token is the expected bearer token; an
// empty token leaves the endpoints unauthenticated.
func New(maxRunners int, token string) http.Handler {
	return dispatch.NewHandler(mock.New(maxRunners), dispatch.HandlerOptions{Token: token})
}
