// Package adaptertest is the v1 conformance kit for adapter.Adapter and for
// HTTP controllers that speak the broker external-dispatch contract.
//
// External modules import this package and call RunConformance or
// RunHTTPConformance. Memory is an in-memory fixture for local tests. It is
// a contract reference, not a production runner backend.
package adaptertest
