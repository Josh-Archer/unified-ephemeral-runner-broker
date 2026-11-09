package httpdispatch

import (
	"net/http/httptest"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/adaptertest"
)

func TestHTTPDispatchConformance(t *testing.T) {
	server := httptest.NewServer(New(1, "test-token"))
	t.Cleanup(server.Close)
	adaptertest.RunHTTPConformance(t, server.URL, adaptertest.HTTPOptions{
		Token:   "test-token",
		Backend: "mock",
	})
}
