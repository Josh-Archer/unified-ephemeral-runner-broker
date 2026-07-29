package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type mapSecrets map[string]map[string]string

func (m mapSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	data, ok := m[name]
	if !ok {
		return nil, context.Canceled
	}
	return data, nil
}

func TestDispatcherDeliversSignedReadyEvent(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = body
		gotHeaders = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	secret := "test-signing-secret"
	d := New(model.WebhooksConfig{
		Enabled:     true,
		MaxAttempts: 1,
		Timeout:     time.Second,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           server.URL,
			SigningSecret: secret,
		}},
	}, nil, server.Client())
	d.sleep = func(time.Duration) {}

	status := model.AllocationStatus{
		ID:              "alloc-1",
		Pool:            model.PoolFull,
		SelectedBackend: model.BackendARC,
		State:           model.StateReady,
		RunnerLabel:     "runner-1",
	}
	d.Notify(status)
	d.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(gotBody) == 0 {
		t.Fatal("expected webhook body")
	}
	if gotHeaders.Get(headerEvent) != "allocation.ready" {
		t.Fatalf("event header = %q", gotHeaders.Get(headerEvent))
	}
	if gotHeaders.Get(headerDelivery) == "" {
		t.Fatal("expected delivery id header")
	}
	if !VerifySignature(secret, gotHeaders.Get(headerSignature), gotBody) {
		t.Fatalf("signature mismatch: %q", gotHeaders.Get(headerSignature))
	}

	var envelope Envelope
	if err := json.Unmarshal(gotBody, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if envelope.Event != "allocation.ready" {
		t.Fatalf("envelope event = %q", envelope.Event)
	}
	if envelope.Allocation.ID != "alloc-1" {
		t.Fatalf("allocation id = %q", envelope.Allocation.ID)
	}
}

func TestDispatcherFiltersEvents(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := New(model.WebhooksConfig{
		Enabled:     true,
		MaxAttempts: 1,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           server.URL,
			SigningSecret: "s",
			Events:        []string{"completed", "failed"},
		}},
	}, nil, server.Client())
	d.sleep = func(time.Duration) {}

	d.Notify(model.AllocationStatus{ID: "a", State: model.StateReady})
	d.Wait()
	if hits.Load() != 0 {
		t.Fatalf("ready should be filtered, hits=%d", hits.Load())
	}

	d.Notify(model.AllocationStatus{ID: "a", State: model.StateCompleted})
	d.Wait()
	if hits.Load() != 1 {
		t.Fatalf("completed should deliver, hits=%d", hits.Load())
	}
}

func TestDispatcherRetriesThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := New(model.WebhooksConfig{
		Enabled:        true,
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           server.URL,
			SigningSecret: "s",
		}},
	}, nil, server.Client())
	d.sleep = func(time.Duration) {}

	d.Notify(model.AllocationStatus{ID: "a", State: model.StateFailed})
	d.Wait()
	if hits.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", hits.Load())
	}
}

func TestDispatcherDoesNotRetryPermanentErrors(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	d := New(model.WebhooksConfig{
		Enabled:     true,
		MaxAttempts: 5,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:           server.URL,
			SigningSecret: "s",
		}},
	}, nil, server.Client())
	d.sleep = func(time.Duration) {}

	d.Notify(model.AllocationStatus{ID: "a", State: model.StateCanceled})
	d.Wait()
	if hits.Load() != 1 {
		t.Fatalf("expected single attempt for 403, got %d", hits.Load())
	}
}

func TestDispatcherResolvesSecretRef(t *testing.T) {
	var verified atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if VerifySignature("from-k8s", r.Header.Get(headerSignature), body) {
			verified.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	secrets := mapSecrets{
		"uecb-webhook": {"signing_secret": "from-k8s"},
	}
	d := New(model.WebhooksConfig{
		Enabled:     true,
		MaxAttempts: 1,
		Endpoints: []model.WebhookEndpointConfig{{
			URL:              server.URL,
			SigningSecretRef: "uecb-webhook",
		}},
	}, secrets, server.Client())
	d.sleep = func(time.Duration) {}

	d.Notify(model.AllocationStatus{ID: "a", State: model.StateExpired})
	d.Wait()
	if !verified.Load() {
		t.Fatal("expected signature using secret from secretRef")
	}
}

func TestEventName(t *testing.T) {
	if EventName(model.StateReady) != EventReady {
		t.Fatal("ready")
	}
	if EventName(model.StatePending) != "" {
		t.Fatal("pending should not be webhooked")
	}
	if EventName(model.StateQuarantined) != "" {
		t.Fatal("quarantined should not be webhooked")
	}
}
