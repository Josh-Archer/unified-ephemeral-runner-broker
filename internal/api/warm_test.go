package api

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestEffectiveWarmBoundsWithoutSchedule(t *testing.T) {
	cfg := model.BackendConfig{WarmMin: 1, WarmMax: 3}
	min, max, err := effectiveWarmBounds(cfg, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if min != 1 || max != 3 {
		t.Fatalf("got min=%d max=%d, want 1/3", min, max)
	}
}

func TestEffectiveWarmBoundsOutsideWindowIsZero(t *testing.T) {
	cfg := model.BackendConfig{
		WarmMin: 2,
		WarmMax: 4,
		WarmSchedule: &model.WarmScheduleConfig{
			Timezone: "UTC",
			Windows: []model.WarmWindowConfig{{
				Days:  []string{"mon"},
				Start: "09:00",
				End:   "17:00",
			}},
		},
	}
	// Tuesday
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	min, max, err := effectiveWarmBounds(cfg, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if min != 0 || max != 0 {
		t.Fatalf("outside window want 0/0, got %d/%d", min, max)
	}
}

func TestEffectiveWarmBoundsWindowOverride(t *testing.T) {
	warmMin := 1
	warmMax := 2
	cfg := model.BackendConfig{
		WarmMin: 0,
		WarmMax: 0,
		WarmSchedule: &model.WarmScheduleConfig{
			Timezone: "UTC",
			Windows: []model.WarmWindowConfig{{
				Start:   "00:00",
				End:     "23:59",
				WarmMin: &warmMin,
				WarmMax: &warmMax,
			}},
		},
	}
	min, max, err := effectiveWarmBounds(cfg, time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if min != 1 || max != 2 {
		t.Fatalf("want window override 1/2, got %d/%d", min, max)
	}
}

func TestWarmWindowCrossesMidnight(t *testing.T) {
	local := time.Date(2026, 7, 28, 23, 30, 0, 0, time.UTC)
	active, err := warmWindowActive(model.WarmWindowConfig{Start: "22:00", End: "06:00"}, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !active {
		t.Fatal("expected 23:30 to be inside 22:00-06:00 window")
	}
	local = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	active, err = warmWindowActive(model.WarmWindowConfig{Start: "22:00", End: "06:00"}, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("expected 12:00 to be outside 22:00-06:00 window")
	}
}
