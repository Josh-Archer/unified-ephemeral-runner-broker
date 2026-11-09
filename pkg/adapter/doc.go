// Package adapter is the frozen v1 contract for provider-owned runner
// controllers. APIVersion is "v1".
//
// The Adapter method set, allocation state strings, and the field semantics
// in docs/adapter-sdk.md stay stable for the life of v1. Later v1 releases
// may add optional struct fields and helper packages. A breaking change
// requires a new major API version.
//
// In-process controllers implement Adapter and prove it with
// pkg/adapter/adaptertest. Out-of-process controllers speak the
// external-dispatch HTTP contract served by pkg/adapter/dispatch. The broker
// calls that HTTP contract through the built-in external backends; it does
// not load out-of-tree Go plugins.
package adapter
