package adapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestAPIVersionIsV1(t *testing.T) {
	if APIVersion != "v1" {
		t.Fatalf("APIVersion = %q, want %q; a new major API version belongs in a new package", APIVersion, "v1")
	}
}

func TestAdapterV1MethodSet(t *testing.T) {
	typ := reflect.TypeOf((*Adapter)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	want := map[string]struct {
		in  []reflect.Type
		out []reflect.Type
	}{
		"Health": {
			in:  []reflect.Type{ctxType},
			out: []reflect.Type{reflect.TypeOf(HealthStatus{}), reflect.TypeOf((*error)(nil)).Elem()},
		},
		"Capacity": {
			in:  []reflect.Type{ctxType},
			out: []reflect.Type{reflect.TypeOf(CapacityStatus{}), reflect.TypeOf((*error)(nil)).Elem()},
		},
		"Reserve": {
			in:  []reflect.Type{ctxType, reflect.TypeOf(ReservationRequest{})},
			out: []reflect.Type{reflect.TypeOf(Reservation{}), reflect.TypeOf((*error)(nil)).Elem()},
		},
		"Launch": {
			in:  []reflect.Type{ctxType, reflect.TypeOf(LaunchRequest{})},
			out: []reflect.Type{reflect.TypeOf(LaunchResult{}), reflect.TypeOf((*error)(nil)).Elem()},
		},
		"Cleanup": {
			in:  []reflect.Type{ctxType, reflect.TypeOf(CleanupRequest{})},
			out: []reflect.Type{reflect.TypeOf((*error)(nil)).Elem()},
		},
	}
	if typ.NumMethod() != len(want) {
		t.Fatalf("Adapter method count = %d, want %d", typ.NumMethod(), len(want))
	}
	for name, sig := range want {
		method, ok := typ.MethodByName(name)
		if !ok {
			t.Fatalf("Adapter is missing %s", name)
		}
		if method.Type.NumIn() != len(sig.in) || method.Type.NumOut() != len(sig.out) {
			t.Fatalf("%s signature in=%d out=%d, want in=%d out=%d", name, method.Type.NumIn(), method.Type.NumOut(), len(sig.in), len(sig.out))
		}
		for i, wantType := range sig.in {
			if got := method.Type.In(i); got != wantType {
				t.Fatalf("%s In(%d) = %s, want %s", name, i, got, wantType)
			}
		}
		for i, wantType := range sig.out {
			if got := method.Type.Out(i); got != wantType {
				t.Fatalf("%s Out(%d) = %s, want %s", name, i, got, wantType)
			}
		}
	}
}

func TestAllocationStateV1Values(t *testing.T) {
	got := map[AllocationState]string{
		StateReserved:  string(StateReserved),
		StateReady:     string(StateReady),
		StatePending:   string(StatePending),
		StateCanceled:  string(StateCanceled),
		StateExpired:   string(StateExpired),
		StateFailed:    string(StateFailed),
		StateCompleted: string(StateCompleted),
	}
	want := map[AllocationState]string{
		StateReserved:  "reserved",
		StateReady:     "ready",
		StatePending:   "pending",
		StateCanceled:  "canceled",
		StateExpired:   "expired",
		StateFailed:    "failed",
		StateCompleted: "completed",
	}
	if len(got) != len(want) {
		t.Fatalf("state count = %d, want %d", len(got), len(want))
	}
	for state, value := range want {
		if got[state] != value {
			t.Fatalf("state %q = %q, want %q", state, got[state], value)
		}
	}
}

func TestFreeSlots(t *testing.T) {
	if got := FreeSlots(CapacityStatus{MaxRunners: 5, ActiveRunners: 2, PendingRunners: 1, WarmRunners: 1}); got != 1 {
		t.Fatalf("FreeSlots = %d, want 1", got)
	}
	if got := FreeSlots(CapacityStatus{MaxRunners: 1, ActiveRunners: 3}); got != 0 {
		t.Fatalf("FreeSlots overcommit = %d, want 0", got)
	}
}

func TestV1GuidePublished(t *testing.T) {
	root := moduleRoot(t)
	guide, err := os.ReadFile(filepath.Join(root, "docs", "third-party-backend.md"))
	if err != nil {
		t.Fatalf("read third-party backend guide: %v", err)
	}
	for _, needle := range []string{
		"adapter.APIVersion",
		"v1",
		"adaptertest.RunConformance",
		"adaptertest.RunHTTPConformance",
		"dispatch.NewHandler",
		"ErrCapacityExhausted",
		"dispatch_url",
		"cleanup_url",
		"capacity_url",
		"health_url",
		"registrationTimeout",
		"20s",
		"90s",
	} {
		if !strings.Contains(string(guide), needle) {
			t.Errorf("docs/third-party-backend.md missing %q", needle)
		}
	}

	sdk, err := os.ReadFile(filepath.Join(root, "docs", "adapter-sdk.md"))
	if err != nil {
		t.Fatalf("read adapter-sdk.md: %v", err)
	}
	for _, needle := range []string{"APIVersion", "third-party-backend.md", "v1"} {
		if !strings.Contains(string(sdk), needle) {
			t.Errorf("docs/adapter-sdk.md missing %q", needle)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve adapter package path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
