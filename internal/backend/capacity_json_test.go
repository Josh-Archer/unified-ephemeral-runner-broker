package backend

import (
	"strings"
	"testing"
)

func TestCapacityStatusFromJSONFreeSlotsReconstruction(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		FreeSlots:     3,
		ActiveRunners: 1,
	})
	if status.MaxRunners != 4 || FreeSlots(status) != 3 {
		t.Fatalf("expected max=4 free=3, got %+v free=%d", status, FreeSlots(status))
	}
}

func TestCapacityStatusFromJSONExhaustionWithoutMax(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		ActiveRunners:  2,
		PendingRunners: 1,
		FreeSlots:      0,
	})
	if status.MaxRunners != 3 || FreeSlots(status) != 0 {
		t.Fatalf("expected full reconstruction max=3 free=0, got %+v free=%d", status, FreeSlots(status))
	}
}

func TestDecodeCapacityJSON(t *testing.T) {
	status, err := DecodeCapacityJSON(strings.NewReader(`{"max_runners":5,"active_runners":2,"pending_runners":1,"warm_runners":1}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.MaxRunners != 5 || FreeSlots(status) != 1 {
		t.Fatalf("unexpected status %+v free=%d", status, FreeSlots(status))
	}
}
