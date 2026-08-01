package fakecapacity_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity/fakecapacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestBackendCapacityAndMutation(t *testing.T) {
	b := fakecapacity.NewBackend(model.BackendCodeBuild, fakecapacity.Free(4, 2))
	if b.Name() != model.BackendCodeBuild {
		t.Fatalf("name: got %s", b.Name())
	}
	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 4 || backend.FreeSlots(status) != 2 {
		t.Fatalf("unexpected status %+v", status)
	}

	b.SetStatus(fakecapacity.Full(4))
	status, err = b.Capacity(context.Background())
	if err != nil || backend.FreeSlots(status) != 0 {
		t.Fatalf("expected full after SetStatus, got %+v err=%v", status, err)
	}

	b.SetError(io.EOF)
	if _, err := b.Capacity(context.Background()); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	b.SetError(nil)
	b.SetStatus(fakecapacity.Free(3, 1))
	status, err = b.Capacity(context.Background())
	if err != nil || status.MaxRunners != 3 {
		t.Fatalf("expected recovery, got %+v err=%v", status, err)
	}
}

func TestMapReporter(t *testing.T) {
	b := fakecapacity.NewBackend(model.BackendLambda, fakecapacity.Full(1))
	reporter := fakecapacity.MapReporter{model.BackendLambda: b}

	got, ok := reporter.CapacityBackend(model.BackendLambda)
	if !ok || got == nil {
		t.Fatal("expected lambda reporter")
	}
	if _, ok := reporter.CapacityBackend(model.BackendARC); ok {
		t.Fatal("arc should be missing")
	}
	if _, ok := fakecapacity.MapReporter(nil).CapacityBackend(model.BackendLambda); ok {
		t.Fatal("nil map should miss")
	}
}

func TestServerJSONProtocol(t *testing.T) {
	srv := fakecapacity.NewServer(t,
		fakecapacity.WithStatus(fakecapacity.Detailed(5, 2, 1, 1)),
		fakecapacity.WithBearerAuth("broker-secret"),
	)

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer broker-secret")
	req.Header.Set("X-UECB-Backend", "codebuild")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var payload map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_runners"] != 5 || payload["active_runners"] != 2 ||
		payload["pending_runners"] != 1 || payload["warm_runners"] != 1 {
		t.Fatalf("unexpected payload %+v", payload)
	}

	// Unauthorized without bearer.
	resp2, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp2.StatusCode)
	}
}

func TestServerProbeFailureAndMutation(t *testing.T) {
	srv := fakecapacity.NewServer(t, fakecapacity.WithStatus(fakecapacity.Free(2, 2)))
	srv.SetStatusCode(http.StatusServiceUnavailable)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	srv.SetStatusCode(http.StatusOK)
	srv.SetStatus(fakecapacity.Full(2))
	resp2, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var payload map[string]int
	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_runners"] != 2 || payload["active_runners"] != 2 {
		t.Fatalf("unexpected payload after recovery %+v", payload)
	}
}

func TestServerFreeSlotsOnly(t *testing.T) {
	srv := fakecapacity.NewServer(t, fakecapacity.WithFreeSlotsOnly(3, 1, 0, 0))
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["free_slots"] != 3 || payload["max_runners"] != 0 {
		t.Fatalf("expected free_slots reconstruction fixture, got %+v", payload)
	}
}

func TestSnapshotHelpers(t *testing.T) {
	now := time.Now().UTC()
	live := fakecapacity.LiveSnapshot(model.BackendARC, fakecapacity.Free(2, 1), now)
	if live.Source != "live" || live.Stale {
		t.Fatalf("live snapshot: %+v", live)
	}
	stale := fakecapacity.StaleSnapshot(model.BackendARC, fakecapacity.Full(1), now)
	if !stale.Stale || stale.Source != "live" {
		t.Fatalf("stale snapshot: %+v", stale)
	}
	prev := fakecapacity.Free(8, 4)
	errSnap := fakecapacity.ErrorSnapshot(model.BackendARC, "timeout", &prev, now)
	if errSnap.Source != "error" || errSnap.Status.MaxRunners != 8 {
		t.Fatalf("error snapshot: %+v", errSnap)
	}

	manager := capacity.NewManager()
	fakecapacity.SeedManager(manager, live)
	if _, ok := manager.Get(model.BackendARC); !ok {
		t.Fatal("seed manager failed")
	}
}
