// Package fakecapacity provides in-process Capacity() and capacity_url fixtures
// for local development and unit tests of live-capacity routing.
//
// Use these helpers to exercise skip-exhausted, fallback, stale pass-through,
// and block policies without network access to AWS, Azure, or GCP.
//
// Typical patterns:
//
//	// Seed the capacity manager directly (allocation-path unit tests).
//	manager.Set(fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Full(2), time.Now().UTC()))
//
//	// In-process CapacityBackend for refresh-loop tests.
//	reporter := fakecapacity.MapReporter{
//	    model.BackendCodeBuild: fakecapacity.NewBackend(model.BackendCodeBuild, fakecapacity.Free(4, 1)),
//	}
//	capacity.Refresh(ctx, manager, reporter, cfg, time.Now().UTC())
//
//	// HTTP capacity_url server for external-dispatch integration tests.
//	srv := fakecapacity.NewServer(t, fakecapacity.WithStatus(fakecapacity.Free(5, 2)))
//	// wire srv.URL into the backend secret as capacity_url
package fakecapacity
