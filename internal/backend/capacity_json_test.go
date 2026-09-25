package backend

import (
	"strings"
	"testing"
)

func intPtr(v int) *int {
	return &v
}

func TestCapacityStatusFromJSONFreeSlotsReconstruction(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		FreeSlots:     intPtr(3),
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
		FreeSlots:      intPtr(0),
	})
	if status.MaxRunners != 3 || FreeSlots(status) != 0 {
		t.Fatalf("expected full reconstruction max=3 free=0, got %+v free=%d", status, FreeSlots(status))
	}
}

func TestCapacityStatusFromJSONFreeSlotsWithMaxRunners(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		MaxRunners:    10,
		ActiveRunners: 2,
		FreeSlots:     intPtr(3),
	})
	if status.MaxRunners != 5 || FreeSlots(status) != 3 {
		t.Fatalf("expected max=5 free=3, got %+v free=%d", status, FreeSlots(status))
	}
}

func TestCapacityStatusFromJSONFreeSlotsZeroWithMaxRunners(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		MaxRunners:     10,
		ActiveRunners:  2,
		PendingRunners: 1,
		FreeSlots:      intPtr(0),
	})
	if status.MaxRunners != 3 || FreeSlots(status) != 0 {
		t.Fatalf("expected max=3 free=0, got %+v free=%d", status, FreeSlots(status))
	}
}

func TestCapacityStatusFromJSONFreeSlotsExceedingMaxRunners(t *testing.T) {
	status := CapacityStatusFromJSON(CapacityJSON{
		MaxRunners:    5,
		ActiveRunners: 2,
		FreeSlots:     intPtr(10),
	})
	if status.MaxRunners != 5 || FreeSlots(status) != 3 {
		t.Fatalf("expected max=5 free=3, got %+v free=%d", status, FreeSlots(status))
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

func TestDecodeCapacityJSONWithFreeSlotsAndMax(t *testing.T) {
	status, err := DecodeCapacityJSON(strings.NewReader(`{"max_runners":10,"active_runners":2,"pending_runners":1,"warm_runners":0,"free_slots":3}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.MaxRunners != 6 || FreeSlots(status) != 3 {
		t.Fatalf("unexpected status %+v free=%d", status, FreeSlots(status))
	}
}

func TestDecodeCapacityJSONWithFreeSlotsZeroAndMax(t *testing.T) {
	status, err := DecodeCapacityJSON(strings.NewReader(`{"max_runners":10,"active_runners":2,"pending_runners":1,"warm_runners":0,"free_slots":0}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.MaxRunners != 3 || FreeSlots(status) != 0 {
		t.Fatalf("unexpected status %+v free=%d", status, FreeSlots(status))
	}
}
