package externaldispatch

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/examples/adapter/mock"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	sdk "github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch"
)

func TestDispatchRequestJSONMatchesAdapterSDKV1(t *testing.T) {
	original := dispatchRequest{
		Action:            "dispatch",
		Backend:           "codebuild",
		AllocationID:      "alloc-1",
		Pool:              "lite",
		RunnerLabel:       "uecb-codebuild-alloc-1",
		RunnerName:        "uecb-codebuild-alloc-1",
		RunnerLabels:      []string{"self-hosted", "linux", "uecb-codebuild-alloc-1"},
		RequestedLabels:   []string{"linux"},
		LaunchMode:        "warm",
		JobTimeout:        "15m0s",
		JobTimeoutSeconds: 900,
		GitHub: dispatchGitHub{
			ScopeType:    "repository",
			Organization: "example-org",
			Owner:        "example-org",
			Repository:   "example-org/example-repo",
			TargetURL:    "https://github.com/example-org/example-repo",
			RunnerGroup:  "default",
		},
	}
	assertJSONRoundTrip(t, original)
}

func TestDispatchResponseJSONMatchesAdapterSDKV1(t *testing.T) {
	original := dispatchResponse{
		RunnerLabel: "uecb-codebuild-alloc-1",
		Metadata:    map[string]string{"execution_id": "run-1", "provider": "example"},
		StatusURL:   "https://example.invalid/status",
		DetailsURL:  "https://example.invalid/details",
		ExecutionID: "run-1",
	}
	assertJSONRoundTrip(t, original)
}

func TestCleanupRequestJSONMatchesAdapterSDKV1(t *testing.T) {
	original := cleanupRequest{
		Action:        "cleanup",
		Backend:       "codebuild",
		AllocationID:  "alloc-1",
		CorrelationID: "corr-1",
		Pool:          "lite",
		RunnerLabel:   "uecb-codebuild-alloc-1",
		State:         "completed",
		Error:         "job finished",
		Metadata:      map[string]string{"execution_id": "run-1"},
	}
	assertJSONRoundTrip(t, original)
}

func TestCapacityJSONMatchesAdapterSDKV1(t *testing.T) {
	free := 3
	original := backend.CapacityJSON{
		MaxRunners:     10,
		ActiveRunners:  4,
		PendingRunners: 1,
		WarmRunners:    2,
		FreeSlots:      &free,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal broker capacity: %v", err)
	}
	var public sdk.CapacityBody
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatalf("unmarshal public capacity: %v", err)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal public capacity: %v", err)
	}
	var round backend.CapacityJSON
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal broker capacity: %v", err)
	}
	if !reflect.DeepEqual(original, round) {
		t.Fatalf("capacity JSON round trip = %#v, want %#v", round, original)
	}
}

func TestSDKHandlerServesBrokerExternalDispatch(t *testing.T) {
	const token = "broker-secret"
	server := httptest.NewServer(sdk.NewHandler(mock.New(1), sdk.HandlerOptions{Token: token}))
	t.Cleanup(server.Close)

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL:   server.URL + sdk.PathDispatch,
			secretKeyCleanupURL:    server.URL + sdk.PathCleanup,
			secretKeyHealthURL:     server.URL + sdk.PathHealth,
			secretKeyCapacityURL:   server.URL + sdk.PathCapacity,
			secretKeyDispatchToken: token,
		},
	})

	pool, backendCfg := liteCodeBuild(t, cfg)
	ctx := context.Background()
	if err := dispatchBackend.Probe(ctx, pool, backendCfg); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	before, err := dispatchBackend.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	provisioned, err := dispatchBackend.Provision(ctx, model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:       "abc123",
		Pool:     model.PoolLite,
		Metadata: map[string]string{"launch_mode": "cold"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if provisioned.RunnerLabel != "uecb-codebuild-abc123" {
		t.Fatalf("runner label = %q", provisioned.RunnerLabel)
	}
	if provisioned.Metadata["github.target_url"] != "https://github.com/example-org/example-repo" {
		t.Fatalf("dispatch metadata = %+v", provisioned.Metadata)
	}
	if provisioned.Metadata["launch_mode"] != "cold" {
		t.Fatalf("launch_mode = %q", provisioned.Metadata["launch_mode"])
	}
	if provisioned.Metadata["adapter"] != "mock" {
		t.Fatalf("adapter metadata = %+v", provisioned.Metadata)
	}

	mid, err := dispatchBackend.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity after provision: %v", err)
	}
	if got, want := usedCapacity(mid), usedCapacity(before)+1; got != want {
		t.Fatalf("used slots after provision = %d, want %d (%+v)", got, want, mid)
	}

	_, err = dispatchBackend.Provision(ctx, model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "abc124",
		Pool: model.PoolLite,
	})
	if !backend.IsCapacityExhausted(err) {
		t.Fatalf("second Provision error = %v, want capacity exhausted", err)
	}

	if err := dispatchBackend.Cleanup(ctx, model.AllocationStatus{
		ID:          "abc123",
		Pool:        model.PoolLite,
		State:       model.StateCompleted,
		RunnerLabel: provisioned.RunnerLabel,
	}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after, err := dispatchBackend.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity after cleanup: %v", err)
	}
	if got, want := usedCapacity(after), usedCapacity(before); got != want {
		t.Fatalf("used slots after cleanup = %d, want %d (%+v)", got, want, after)
	}
}

func liteCodeBuild(t *testing.T, cfg model.BrokerConfig) (model.PoolConfig, model.BackendConfig) {
	t.Helper()
	for _, pool := range cfg.Pools {
		if pool.Name != model.PoolLite {
			continue
		}
		backendCfg, ok := pool.Backends[model.BackendCodeBuild]
		if !ok {
			t.Fatal("lite pool has no codebuild backend")
		}
		return pool, backendCfg
	}
	t.Fatal("config has no lite pool")
	return model.PoolConfig{}, model.BackendConfig{}
}

func usedCapacity(status backend.CapacityStatus) int {
	return status.ActiveRunners + status.PendingRunners + status.WarmRunners
}

func assertJSONRoundTrip(t *testing.T, original any) {
	t.Helper()
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	var public any
	switch original.(type) {
	case dispatchRequest:
		public = &sdk.Request{}
	case dispatchResponse:
		public = &sdk.Response{}
	case cleanupRequest:
		public = &sdk.CleanupRequest{}
	default:
		t.Fatalf("unsupported payload %T", original)
	}
	if err := json.Unmarshal(raw, public); err != nil {
		t.Fatalf("unmarshal public view: %v", err)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal public view: %v", err)
	}
	round := reflect.New(reflect.TypeOf(original)).Interface()
	if err := json.Unmarshal(encoded, round); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	got := reflect.ValueOf(round).Elem().Interface()
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("JSON round trip\n got %#v\nwant %#v\nbroker JSON %s\npublic JSON %s", got, original, raw, encoded)
	}
}
