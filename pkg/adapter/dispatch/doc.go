// Package dispatch serves adapter API v1 over the broker's external-dispatch
// HTTP contract.
//
// A third-party controller implements adapter.Adapter, passes
// adaptertest.RunConformance, and exposes it with NewHandler. The broker
// reaches the handler through dispatch_url, cleanup_url, health_url, and
// capacity_url on an existing external backend secret. JSON objects in this
// package match that contract: unknown fields are ignored, and the field
// names below are part of v1.
package dispatch
